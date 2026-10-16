package overlayfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMetadataPatternEligibility(t *testing.T) {
	for _, tt := range []struct {
		glob, name string
		cache      bool
	}{
		{"*.md", "package.json", true},
		{"*.md", "AGENTS.md", false},
		{"{notes,docs}/**", "package.json", true},
		{"{notes,docs}/**", "notes/file.txt", false},
		{"docs-*/guide.md", "src/main.go", true},
		{"docs-*/guide.md", "docs-api", false},
		{"docs-*/guide.md", "docs-api/other.txt", true},
		{"[ab]/*.md", "c/file.txt", true},
		{"[ab]/*.md", "a", false},
		{"**/*.md", "src/file.txt", false},                // It could become an overlay directory.
		{"{docs/file,notes/file}", "package.json", false}, // Conservative for alternatives spanning components.
		{"**/*", "package.json", false},
	} {
		t.Run(tt.glob+"/"+tt.name, func(t *testing.T) {
			v, root, _, _, source := mountedMetadata(t, tt.glob, tt.name)
			var out fuse.EntryOut
			if _, errno := root.Lookup(context.Background(), tt.name, &out); errno != 0 {
				t.Fatal(errno)
			}
			if got := out.AttrTimeout() > 0; got != tt.cache {
				t.Fatalf("attribute caching=%v, want %v", got, tt.cache)
			}
			// A currently absent contribution is still allowed to appear later.
			if !tt.cache {
				scratch.Write(t, filepath.Join(source, tt.name), "overlay")
				if err := v.RefreshPaths(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestNativeReadCacheAcrossOpens(t *testing.T) {
	for _, glob := range []string{"*.md", "**/*"} {
		t.Run(glob, func(t *testing.T) { checkNativeReadCacheAcrossOpens(t, glob) })
	}
}

func checkNativeReadCacheAcrossOpens(t *testing.T, glob string) {
	t.Helper()
	v, root, raw, target, _ := mountedMetadata(t, glob, "file.txt")
	name := filepath.Join(target, "file.txt")
	readNative(t, name, "base")
	before := raw.reads.Load()
	for range 10 {
		readNative(t, name, "base")
	}
	if got := raw.reads.Load() - before; got != 0 {
		t.Fatalf("unchanged file required %d READs across 10 opens", got)
	}
	info, err := v.Project().Stat("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Project().WriteFile("file.txt", []byte("edit"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := v.Project().Chtimes("file.txt", info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	// Same size and restored mtime must not retain the old page contents.
	v.cache.event(metadataEvent(v, root, "file.txt", fsnotify.Write))
	readNative(t, name, "edit")
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer closeNativeTestFile(t, file)
	if err := v.Project().WriteFile("file.txt", []byte("live"), 0644); err != nil {
		t.Fatal(err)
	}
	v.cache.event(metadataEvent(v, root, "file.txt", fsnotify.Write))
	data := make([]byte, 4)
	if _, err := file.ReadAt(data, 0); err != nil || string(data) != "live" {
		t.Fatalf("open descriptor after backing edit: %q %v", data, err)
	}
}

func TestNativeReadCacheBeforeOverlayAppears(t *testing.T) {
	v, _, raw, target, source := mountedMetadata(t, "**/*", "merged.txt")
	name := filepath.Join(target, "merged.txt")
	readNative(t, name, "base")
	before := raw.reads.Load()
	for range 10 {
		readNative(t, name, "base")
	}
	if got := raw.reads.Load() - before; got != 0 {
		t.Fatalf("unchanged project-only file required %d READs", got)
	}
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer closeNativeTestFile(t, file)
	scratch.Write(t, filepath.Join(source, "merged.txt"), "overlay")
	scratch.Write(t, filepath.Join(source, "only.txt"), "overlay")
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	verifyMetadataCallers(t, target)
	data := make([]byte, 4)
	if _, err := file.ReadAt(data, 0); err != nil || string(data) != "base" {
		t.Fatalf("earlier descriptor changed view: %q %v", data, err)
	}
}

func TestNativeCacheInvalidationAfterEntryExpiry(t *testing.T) {
	for _, evict := range []bool{false, true} {
		t.Run(fmt.Sprintf("evict=%v", evict), func(t *testing.T) {
			v, root, raw, target, _ := mountedMetadata(t, "**/*", "file.txt")
			file, err := os.Open(filepath.Join(target, "file.txt"))
			if err != nil {
				t.Fatal(err)
			}
			defer closeNativeTestFile(t, file)
			data := make([]byte, 4)
			if _, err := file.ReadAt(data, 0); err != nil {
				t.Fatal(err)
			}
			v.cache.mu.Lock()
			dir := v.cache.dirs[&root.Inode]
			past := time.Now().Add(-metadataTimeout)
			dir.entries["file.txt"] = past
			for i := range metadataEntryLimit {
				dir.entries[fmt.Sprintf("expired-%d", i)] = past
			}
			dir.remember("new-entry") // Prune expired metadata while pages remain cached.
			v.cache.mu.Unlock()
			before := raw.reads.Load()
			if evict {
				v.cache.evictOldest()
			} else {
				v.cache.invalidateAll()
			}
			if _, err := file.ReadAt(data, 0); err != nil {
				t.Fatal(err)
			}
			if raw.reads.Load() == before {
				t.Fatal("cached pages survived loss of watch coverage")
			}
		})
	}
}

func closeNativeTestFile(t *testing.T, file *os.File) {
	t.Helper()
	if err := file.Close(); err != nil {
		t.Error(err)
	}
}

func releaseNativeTestFile(t *testing.T, h fs.FileHandle) {
	t.Helper()
	if errno := h.(fs.FileReleaser).Release(context.Background()); errno != 0 {
		t.Error(errno)
	}
}

func TestNativePassthroughEligibility(t *testing.T) {
	for _, tt := range []struct {
		glob string
		want bool
	}{{"*.md", true}, {"**/*", false}} {
		t.Run(tt.glob, func(t *testing.T) {
			_, root, _, _, _ := mountedMetadata(t, tt.glob, "file.txt")
			var out fuse.EntryOut
			inode, errno := root.Lookup(context.Background(), "file.txt", &out)
			if errno != 0 {
				t.Fatal(errno)
			}
			root.AddChild("file.txt", inode, true)
			h, _, errno := inode.Operations().(*node).Open(context.Background(), syscall.O_RDONLY)
			if errno != 0 {
				t.Fatal(errno)
			}
			defer releaseNativeTestFile(t, h)
			_, eligible := h.(fs.FilePassthroughFder).PassthroughFd()
			if eligible != tt.want {
				t.Fatalf("passthrough eligibility=%v, want %v", eligible, tt.want)
			}
		})
	}
}

func TestNativeCacheVersionAndBackingIdentity(t *testing.T) {
	v, root, _, _, _ := mountedMetadata(t, "*.md", "file.txt")
	var out fuse.EntryOut
	inode, errno := root.Lookup(context.Background(), "file.txt", &out)
	if errno != 0 {
		t.Fatal(errno)
	}
	root.AddChild("file.txt", inode, true)
	n := inode.Operations().(*node)
	open := func(wantCache, wantPassthrough bool) {
		t.Helper()
		h, flags, errno := n.Open(context.Background(), syscall.O_RDONLY)
		if errno != 0 {
			t.Fatal(errno)
		}
		defer releaseNativeTestFile(t, h)
		_, passthrough := h.(fs.FilePassthroughFder).PassthroughFd()
		if (flags&fuse.FOPEN_KEEP_CACHE != 0) != wantCache || passthrough != wantPassthrough {
			t.Fatalf("open flags=%x passthrough=%v, want keep=%v passthrough=%v", flags, passthrough, wantCache, wantPassthrough)
		}
	}
	open(false, true)
	open(true, true)
	info, err := v.Project().Stat("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Project().WriteFile("file.txt", []byte("edit"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := v.Project().Chtimes("file.txt", info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	open(false, true)
	open(true, true)
	if err := v.Project().WriteFile("replacement", []byte("next"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := v.Project().Rename("replacement", "file.txt"); err != nil {
		t.Fatal(err)
	}
	// An OPEN arriving after replacement must not reuse the old inode's
	// registered backing ID, even if an older descriptor is still open.
	open(false, false)
}

func TestDirectoryHandleRewind(t *testing.T) {
	_, _, _, target, _ := mountedMetadata(t, "**/*", "directory/a", "directory/b")
	file, err := os.Open(filepath.Join(target, "directory"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeNativeTestFile(t, file)
	first, err := file.Readdirnames(1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first directory entry: %v %v", first, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	all, err := file.Readdirnames(-1)
	if err != nil || len(all) != 2 || all[0] != first[0] {
		t.Fatalf("rewound directory: %v %v", all, err)
	}
}

func TestMetadataWatchCapacityRecovers(t *testing.T) {
	files := make([]string, metadataWatchLimit+2)
	for i := range files {
		files[i] = fmt.Sprintf("directory-%03d/file.txt", i)
	}
	v, root, _, target, _ := mountedMetadata(t, "notes/**", files...)
	for _, name := range files {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Fatal(err)
		}
	}
	last := filepath.Dir(files[len(files)-1])
	parent := root.GetChild(last).Operations().(*node)
	awaitMetadata(t, func() bool {
		var out fuse.EntryOut
		_, errno := parent.Lookup(context.Background(), "missing", &out)
		if errno != syscall.ENOENT {
			t.Fatal(errno)
		}
		return out.EntryTimeout() > 0
	})
	v.cache.mu.Lock()
	defer v.cache.mu.Unlock()
	if len(v.cache.backings) > metadataWatchLimit {
		t.Fatalf("watch bound exceeded: %d", len(v.cache.backings))
	}
}

func TestMetadataClosedCacheRejectsWatches(t *testing.T) {
	project, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v := &View{}
	v.AddProject(project)
	defer v.Close()
	root := &node{view: v, path: "."}
	fs.NewNodeFS(root, mountOptions())
	cache, err := newMetadataCache(v)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer cache.close()
		if !cache.watch(root, "file.txt") {
			t.Fatal("initial watch failed")
		}
	}()
	if cache.watch(root, "file.txt") {
		t.Fatal("closed cache still grants cache coverage")
	}
}

func TestDirectoryEnumerationAvoidsChildLookups(t *testing.T) {
	v, _, raw, target, _ := mountedMetadata(t, "**/*", "directory/first.txt")
	for i := range 128 {
		if err := v.Project().WriteFile(fmt.Sprintf("directory/file-%03d", i), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "directory")); err != nil {
		t.Fatal(err)
	}
	before := raw.lookups.Load()
	entries, err := os.ReadDir(filepath.Join(target, "directory"))
	if err != nil || len(entries) != 129 {
		t.Fatalf("directory entries=%d err=%v", len(entries), err)
	}
	if got := raw.lookups.Load() - before; got > 1 || raw.plus.Load() != 0 {
		t.Fatalf("names-only enumeration needed %d child LOOKUPs and %d READDIRPLUS calls", got, raw.plus.Load())
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err != nil || info.Size() != 4 {
			t.Fatalf("directory stat: %v %v", info, err)
		}
	}
}

func TestMutationKeepsUnrelatedMetadata(t *testing.T) {
	v, root, raw, target, _ := mountedMetadata(t, "notes/**", "hot.txt", "other.txt")
	hot := filepath.Join(target, "hot.txt")
	if _, err := os.Stat(hot); err != nil {
		t.Fatal(err)
	}
	before := raw.lookups.Load()
	if err := os.Remove(filepath.Join(target, "other.txt")); err != nil {
		t.Fatal(err)
	}
	// Deliver the same external removal event synchronously.
	v.cache.event(metadataEvent(v, root, "other.txt", fsnotify.Remove))
	before = max(before, raw.lookups.Load())
	for range 10 {
		if _, err := os.Stat(hot); err != nil {
			t.Fatal(err)
		}
	}
	if got := raw.lookups.Load() - before; got != 0 {
		t.Fatalf("unrelated mutation discarded hot entry %d times", got)
	}
}

func TestSourceRefreshKeepsUnrelatedMetadata(t *testing.T) {
	v, _, raw, target, source := mountedMetadata(t, "notes/**", "hot.txt", "notes/file.txt")
	hot, affected := filepath.Join(target, "hot.txt"), filepath.Join(target, "notes/file.txt")
	for _, name := range []string{hot, affected} {
		if _, err := os.Stat(name); err != nil {
			t.Fatal(err)
		}
	}
	scratch.Write(t, filepath.Join(source, "notes/new.txt"), "overlay")
	before := raw.lookups.Load()
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	// The affected subtree's next lookup proves reconciliation invalidated it.
	awaitMetadata(t, func() bool {
		if _, err := os.Stat(affected); err != nil {
			t.Fatal(err)
		}
		return raw.lookups.Load() > before
	})
	before = raw.lookups.Load()
	for range 10 {
		if _, err := os.Stat(hot); err != nil {
			t.Fatal(err)
		}
	}
	if raw.lookups.Load() != before {
		t.Fatal("source reconciliation discarded an unrelated project entry")
	}
}

func readNative(t *testing.T, name, want string) {
	t.Helper()
	if got := string(scratch.Read(t, name)); got != want {
		t.Fatalf("read %s: %q, want %q", name, got, want)
	}
}
