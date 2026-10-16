package overlayfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

type countedMetadataFS struct {
	fuse.RawFileSystem
	lookups atomic.Int64
	attrs   atomic.Int64
	reads   atomic.Int64
	plus    atomic.Int64
}

func (c *countedMetadataFS) Read(cancel <-chan struct{}, in *fuse.ReadIn, buf []byte) (fuse.ReadResult, fuse.Status) {
	c.reads.Add(1)
	return c.RawFileSystem.Read(cancel, in, buf)
}

func (c *countedMetadataFS) ReadDirPlus(cancel <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	c.plus.Add(1)
	return c.RawFileSystem.ReadDirPlus(cancel, in, out)
}

func (c *countedMetadataFS) Lookup(cancel <-chan struct{}, header *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	c.lookups.Add(1)
	return c.RawFileSystem.Lookup(cancel, header, name, out)
}

func (c *countedMetadataFS) GetAttr(cancel <-chan struct{}, in *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	c.attrs.Add(1)
	return c.RawFileSystem.GetAttr(cancel, in, out)
}

func mountedMetadata(t *testing.T, glob string, files ...string) (*View, *node, *countedMetadataFS, string, string) {
	t.Helper()
	project, source, target := t.TempDir(), t.TempDir(), t.TempDir()
	for _, name := range files {
		scratch.Write(t, filepath.Join(project, name), "base")
	}
	var v View
	for i, dir := range []string{project, source} {
		r, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			v.AddProject(r)
		} else {
			v.AddOverlay(r, "source", glob)
		}
	}
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	root := &node{view: &v, path: "."}
	opts := mountOptions()
	// Count file caching independently of root-directory permission checks.
	// BenchmarkOperations uses the complete production mount options.
	opts.Options = nil
	c, err := newMetadataCache(&v)
	if err != nil {
		t.Fatal(err)
	}
	v.cache = c
	raw := &countedMetadataFS{RawFileSystem: &metadataFS{fs.NewNodeFS(root, opts)}}
	server, err := fuse.NewServer(raw, target, &opts.MountOptions)
	if err != nil {
		v.Close()
		t.Fatal(err)
	}
	go server.Serve()
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		v.Close()
	})
	if err := server.WaitMount(); err != nil {
		t.Fatal(err)
	}
	return &v, root, raw, target, source
}

func TestMetadataContentEventsPreserveEntries(t *testing.T) {
	for _, op := range []fsnotify.Op{fsnotify.Write, fsnotify.Chmod} {
		t.Run(op.String(), func(t *testing.T) {
			v, root, raw, target, _ := mountedMetadata(t, "notes/**", "file.txt")
			name := filepath.Join(target, "file.txt")
			if _, err := os.Stat(name); err != nil {
				t.Fatal(err)
			}
			beforeLookup, beforeAttr := raw.lookups.Load(), raw.attrs.Load()
			// Deliver the watch event directly so the kernel has processed the
			// invalidation before the next stat, without a scheduling sleep.
			v.cache.event(metadataEvent(v, root, "file.txt", op))
			if info, err := os.Stat(name); err != nil || info.Size() != 4 {
				t.Fatalf("stat after event: %v %v", info, err)
			}
			if got := raw.lookups.Load() - beforeLookup; got != 0 {
				t.Errorf("content event caused %d LOOKUPs, want 0", got)
			}
			if got := raw.attrs.Load() - beforeAttr; got != 1 {
				t.Errorf("content event caused %d GETATTRs, want 1", got)
			}
		})
	}
}

func TestMetadataContentNotificationBudget(t *testing.T) {
	for _, tt := range []struct {
		glob   string
		budget float64
	}{{"notes/**", 12}, {"**/*", 5}} {
		t.Run(tt.glob, func(t *testing.T) {
			v, root, raw, target, _ := mountedMetadata(t, tt.glob, "file.txt")
			name := filepath.Join(target, "file.txt")
			if _, err := os.Stat(name); err != nil {
				t.Fatal(err)
			}
			event := metadataEvent(v, root, "file.txt", fsnotify.Write)
			allocs := testing.AllocsPerRun(100, func() { v.cache.event(event) })
			if allocs > tt.budget {
				t.Errorf("content notification allocated %.0f times, want <=%.0f", allocs, tt.budget)
			}
			before := raw.lookups.Load()
			if err := v.Project().WriteFile("file.txt", []byte("longer content"), 0644); err != nil {
				t.Fatal(err)
			}
			v.cache.event(event)
			if info, err := os.Stat(name); err != nil || info.Size() != int64(len("longer content")) {
				t.Fatalf("stat after write: %v %v", info, err)
			}
			if got := raw.lookups.Load() - before; got != 0 {
				t.Errorf("content notification discarded the entry %d times", got)
			}
		})
	}
}

func metadataEvent(v *View, root *node, name string, op fsnotify.Op) fsnotify.Event {
	v.cache.mu.Lock()
	defer v.cache.mu.Unlock()
	return fsnotify.Event{Name: fmt.Sprintf("/proc/self/fd/%d/%s", v.cache.dirs[&root.Inode].file.Fd(), name), Op: op}
}

func TestMetadataMountedSetattrPreservesEntries(t *testing.T) {
	_, _, raw, target, _ := mountedMetadata(t, "notes/**", "file.txt")
	name := filepath.Join(target, "file.txt")
	if _, err := os.Stat(name); err != nil {
		t.Fatal(err)
	}
	before := raw.lookups.Load()
	for i := range 20 {
		mode := os.FileMode(0600 | i%2*0044)
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
		for range 20 {
			if info, err := os.Stat(name); err != nil || info.Mode().Perm() != mode {
				t.Fatalf("stat after chmod: %v %v, want mode=%o", info, err, mode)
			}
		}
	}
	if got := raw.lookups.Load() - before; got != 0 {
		t.Errorf("chmod discarded the entry %d times, want 0", got)
	}
}

func TestMetadataAttributeQueueSaturationFlushesEntries(t *testing.T) {
	v, root, raw, target, _ := mountedMetadata(t, "notes/**", "file.txt")
	name := filepath.Join(target, "file.txt")
	if _, err := os.Stat(name); err != nil {
		t.Fatal(err)
	}
	before := raw.lookups.Load()
	func() {
		v.cache.mu.Lock()
		defer v.cache.mu.Unlock()
		// Park the notification worker at its cache lock, independently of
		// the event collector. Fill the queue without a scheduling race.
		v.cache.flush <- struct{}{}
		awaitMetadata(t, func() bool { return len(v.cache.flush) == 0 })
		for range metadataAttributeLimit + 1 {
			v.invalidateAttributes(&root.Inode)
		}
		if len(v.cache.attrs) != metadataAttributeLimit || len(v.cache.flush) != 1 {
			t.Fatalf("saturated attributes queue did not request a full flush: attrs=%d flush=%d", len(v.cache.attrs), len(v.cache.flush))
		}
	}()
	awaitMetadata(t, func() bool {
		if _, err := os.Stat(name); err != nil {
			t.Fatal(err)
		}
		return raw.lookups.Load() > before
	})
}

func TestMetadataKernelCaching(t *testing.T) {
	v, _, raw, target, _ := mountedMetadata(t, "notes/**")
	if err := v.Project().WriteFile("package.json", []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// Prime the entry after its creation event; attribute validation is fresh
	// on every cache miss, so a pending notification can only reduce hits.
	for range 5 {
		if _, err := os.Stat(filepath.Join(target, "package.json")); err != nil {
			t.Fatal(err)
		}
	}
	beforeLookup, beforeAttr := raw.lookups.Load(), raw.attrs.Load()
	for range 100 {
		if _, err := os.Stat(filepath.Join(target, "package.json")); err != nil {
			t.Fatal(err)
		}
	}
	if got := raw.lookups.Load() - beforeLookup; got > 1 {
		t.Errorf("100 stats needed %d LOOKUPs, want <=1", got)
	}
	if got := raw.attrs.Load() - beforeAttr; got > 1 {
		t.Errorf("100 stats needed %d GETATTRs, want <=1", got)
	}
	beforeLookup = raw.lookups.Load()
	for range 100 {
		if _, err := os.Stat(filepath.Join(target, "missing.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing: %v", err)
		}
	}
	if got := raw.lookups.Load() - beforeLookup; got != 1 {
		t.Errorf("100 misses needed %d LOOKUPs, want 1", got)
	}
}

func TestMetadataCachePolicy(t *testing.T) {
	for _, glob := range []string{"**/*", "notes/**"} {
		t.Run(glob, func(t *testing.T) {
			v, root, _, _, source := mountedMetadata(t, glob)
			for name, data := range map[string]string{"plain.txt": "base", "notes/merged.txt": "base"} {
				if err := v.Project().MkdirAll(filepath.Dir(name), 0755); err != nil {
					t.Fatal(err)
				}
				if err := v.Project().WriteFile(name, []byte(data), 0644); err != nil {
					t.Fatal(err)
				}
			}
			scratch.Mkdir(t, filepath.Join(source, "notes"))
			scratch.Write(t, filepath.Join(source, "notes/merged.txt"), "overlay")
			scratch.Write(t, filepath.Join(source, "notes/only.txt"), "overlay")
			if err := v.RefreshPaths(); err != nil {
				t.Fatal(err)
			}
			original := readProcessComm
			t.Cleanup(func() { readProcessComm = original })
			for _, caller := range []string{"editor", "git", "git-remote-http", "editor"} {
				readProcessComm = func(uint32) string { return caller }
				ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 123})
				for _, name := range []string{"plain.txt", "notes/merged.txt", "notes/only.txt", "missing.txt"} {
					var out fuse.EntryOut
					_, errno := root.Lookup(ctx, name, &out)
					project := name == "plain.txt" || name == "notes/merged.txt"
					git := caller != "editor"
					if name == "missing.txt" || (git && name == "notes/only.txt") {
						if errno != syscall.ENOENT {
							t.Fatalf("%s %s: %v", caller, name, errno)
						}
					} else if errno != 0 {
						t.Fatal(errno)
					}
					wantEntry := project || (name == "missing.txt" && glob == "notes/**")
					wantAttr := name == "plain.txt" && glob == "notes/**"
					if (out.EntryTimeout() > 0) != wantEntry || (out.AttrTimeout() > 0) != wantAttr {
						t.Errorf("%s %s: entry=%v attr=%v", caller, name, out.EntryTimeout(), out.AttrTimeout())
					}
					if name == "notes/merged.txt" {
						want := uint64(len("baseoverlay"))
						if git {
							want = uint64(len("base"))
						}
						if out.Size != want {
							t.Errorf("%s size=%d want %d", caller, out.Size, want)
						}
					}
				}
			}
		})
	}
}

func awaitMetadata(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("metadata was not invalidated before its one-second TTL expired")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMetadataExternalInvalidation(t *testing.T) {
	v, _, _, target, _ := mountedMetadata(t, "notes/**")
	name := filepath.Join(target, "file.txt")
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := v.Project().WriteFile("file.txt", []byte("one"), 0644); err != nil {
		t.Fatal(err)
	}
	awaitMetadata(t, func() bool { info, err := os.Stat(name); return err == nil && info.Size() == 3 })
	if err := v.Project().WriteFile("file.txt", []byte("longer"), 0644); err != nil {
		t.Fatal(err)
	}
	awaitMetadata(t, func() bool { info, err := os.Stat(name); return err == nil && info.Size() == 6 })
	if err := v.Project().Chmod("file.txt", 0600); err != nil {
		t.Fatal(err)
	}
	awaitMetadata(t, func() bool { info, err := os.Stat(name); return err == nil && info.Mode().Perm() == 0600 })
	if err := v.Project().Rename("file.txt", "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	awaitMetadata(t, func() bool { _, err := os.Stat(name); return errors.Is(err, os.ErrNotExist) })
	name = filepath.Join(target, "renamed.txt")
	if _, err := os.Stat(name); err != nil {
		t.Fatal(err)
	}
	if err := v.Project().Remove("renamed.txt"); err != nil {
		t.Fatal(err)
	}
	awaitMetadata(t, func() bool { _, err := os.Stat(name); return errors.Is(err, os.ErrNotExist) })
}

func TestMetadataWatcherFailureDisablesCaching(t *testing.T) {
	v, root, _, target, _ := mountedMetadata(t, "notes/**")
	if err := v.Project().WriteFile("file.txt", []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "file.txt")); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual watcher's error delivery and fail-closed transition.
	v.cache.watcher.Errors <- fsnotify.ErrClosed
	awaitMetadata(t, func() bool {
		v.cache.mu.Lock()
		defer v.cache.mu.Unlock()
		return v.cache.failed
	})
	for _, name := range []string{"file.txt", "missing.txt"} {
		var out fuse.EntryOut
		_, errno := root.Lookup(context.Background(), name, &out)
		if errno != 0 && errno != syscall.ENOENT {
			t.Fatal(errno)
		}
		if out.EntryTimeout() != 0 || out.AttrTimeout() != 0 {
			t.Errorf("cached %s after watcher failure", name)
		}
	}
}

func TestMetadataEntryLimitFallsBackAndRecovers(t *testing.T) {
	v, root, _, _, _ := mountedMetadata(t, "notes/**")
	for i := range metadataEntryLimit + 1 {
		var out fuse.EntryOut
		_, errno := root.Lookup(context.Background(), fmt.Sprintf("missing-%d", i), &out)
		if errno != syscall.ENOENT {
			t.Fatal(errno)
		}
		if i == metadataEntryLimit && out.EntryTimeout() != 0 {
			t.Fatal("cached a miss beyond the tracking limit")
		}
	}
	// A full directory still serves hot tracked names. Expired names release
	// capacity for new lookups; use their actual expiry state without a sleep.
	var hot fuse.EntryOut
	if _, errno := root.Lookup(context.Background(), "missing-0", &hot); errno != syscall.ENOENT || hot.EntryTimeout() == 0 {
		t.Fatalf("hot entry: %v timeout=%v", errno, hot.EntryTimeout())
	}
	v.cache.mu.Lock()
	for name := range v.cache.dirs[&root.Inode].entries {
		v.cache.dirs[&root.Inode].entries[name] = time.Time{}
	}
	v.cache.mu.Unlock()
	var out fuse.EntryOut
	if _, errno := root.Lookup(context.Background(), "new-miss", &out); errno != syscall.ENOENT || out.EntryTimeout() == 0 {
		t.Fatalf("expired entries did not release capacity: %v timeout=%v", errno, out.EntryTimeout())
	}
}

func TestMetadataSourceTransitionKeepsAttributesUncached(t *testing.T) {
	v, _, _, target, source := mountedMetadata(t, "**/*")
	if err := v.Project().WriteFile("merged.txt", []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(target, "merged.txt")); err != nil || info.Size() != 4 {
		t.Fatalf("prime: %v %v", info, err)
	}
	scratch.Write(t, filepath.Join(source, "merged.txt"), "overlay")
	scratch.Write(t, filepath.Join(source, "only.txt"), "overlay")
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	verifyMetadataCallers(t, target)
}

type metadataResult struct {
	Caller     string
	Size       int64
	HandleSize int64
	Data       string
	Missing    bool
}

func TestMetadataCallerHelper(t *testing.T) {
	target := os.Getenv("OVERLAY_METADATA_TARGET")
	if target == "" {
		return
	}
	// FUSE identifies the calling thread. Keep its comm changes and file
	// operations on that thread, even when the runtime would migrate us.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var results []metadataResult
	for _, caller := range []string{"editor", "git", "git-remote-http", "editor"} {
		if err := os.WriteFile("/proc/thread-self/comm", []byte(caller), 0644); err != nil {
			t.Fatal(err)
		}
		// Keep earlier callers' handles alive. Path stats must not borrow
		// their attributes, while fstat must retain its own descriptor view.
		file, err := os.Open(filepath.Join(target, "merged.txt"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		})
		handleInfo, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(target, "merged.txt"))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(target, "merged.txt"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(filepath.Join(target, "only.txt"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		results = append(results, metadataResult{caller, info.Size(), handleInfo.Size(), string(data), errors.Is(err, os.ErrNotExist)})
	}
	if err := json.NewEncoder(os.Stdout).Encode(results); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestMetadataAlternatingKernelCallers(t *testing.T) {
	v, _, _, target, source := mountedMetadata(t, "**/*")
	if err := v.Project().WriteFile("merged.txt", []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	scratch.Write(t, filepath.Join(source, "merged.txt"), "overlay")
	scratch.Write(t, filepath.Join(source, "only.txt"), "overlay")
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	verifyMetadataCallers(t, target)
}

func TestMetadataDescriptorStatAfterUnlink(t *testing.T) {
	_, _, raw, target, _ := mountedMetadata(t, "**/*", "file.txt")
	name := filepath.Join(target, "file.txt")
	file, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	before := raw.attrs.Load()
	if info, err := file.Stat(); err != nil || info.Size() != 4 {
		t.Fatalf("descriptor stat after unlink: info=%v err=%v", info, err)
	}
	if raw.attrs.Load() <= before {
		t.Fatal("descriptor stat did not reach the FUSE server")
	}
	data := []byte("extended content")
	if _, err := file.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if info, err := file.Stat(); err != nil || info.Size() != int64(len(data)) {
		t.Fatalf("descriptor stat after unlinked write: info=%v err=%v", info, err)
	}
}

func verifyMetadataCallers(t *testing.T, target string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMetadataCallerHelper$")
	cmd.Env = append(os.Environ(), "OVERLAY_METADATA_TARGET="+target)
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("caller helper: %v\n%s", err, data)
	}
	var results []metadataResult
	if err := json.Unmarshal(data, &results); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	if len(results) != 4 {
		t.Fatalf("helper results: %s", data)
	}
	for _, result := range results {
		want := "baseoverlay"
		git := result.Caller != "editor"
		if git {
			want = "base"
		}
		if result.Data != want || result.Size != int64(len(want)) || result.HandleSize != int64(len(want)) || result.Missing != git {
			t.Errorf("caller-dependent view corrupted: %+v, want data=%s size=%d missing=%v", result, want, len(want), git)
		}
	}
}
