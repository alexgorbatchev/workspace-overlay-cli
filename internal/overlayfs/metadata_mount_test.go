package overlayfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
}

func (c *countedMetadataFS) Lookup(cancel <-chan struct{}, header *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	c.lookups.Add(1)
	return c.RawFileSystem.Lookup(cancel, header, name, out)
}

func (c *countedMetadataFS) GetAttr(cancel <-chan struct{}, in *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	c.attrs.Add(1)
	return c.RawFileSystem.GetAttr(cancel, in, out)
}

func mountedMetadata(t *testing.T, glob string) (*View, *node, *countedMetadataFS, string, string) {
	t.Helper()
	project, source, target := t.TempDir(), t.TempDir(), t.TempDir()
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
	opts := &fs.Options{EntryTimeout: new(time.Duration), AttrTimeout: new(time.Duration)}
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
	v.cache.watcher.Errors <- fsnotify.ErrEventOverflow
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
	Caller  string
	Size    int64
	Data    string
	Missing bool
}

func TestMetadataCallerHelper(t *testing.T) {
	target := os.Getenv("OVERLAY_METADATA_TARGET")
	if target == "" {
		return
	}
	var results []metadataResult
	for _, caller := range []string{"editor", "git", "git-remote-http", "editor"} {
		// proc_pid_comm(5) permits a thread to change another thread's comm
		// in its thread group. Address the leader explicitly, as FUSE sends PID.
		if err := os.WriteFile(fmt.Sprintf("/proc/%d/comm", os.Getpid()), []byte(caller), 0644); err != nil {
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
		results = append(results, metadataResult{caller, info.Size(), string(data), errors.Is(err, os.ErrNotExist)})
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
		if result.Data != want || result.Size != int64(len(want)) || result.Missing != git {
			t.Errorf("caller-dependent view corrupted: %+v, want data=%s size=%d missing=%v", result, want, len(want), git)
		}
	}
}
