package overlayfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestDirectoryAttributeCache(t *testing.T) {
	for _, glob := range []string{"notes/**", "**/*"} {
		t.Run(glob, func(t *testing.T) {
			_, _, raw, target, _ := mountedMetadata(t, glob, "directory/file.txt")
			name := filepath.Join(target, "directory")
			// The initial lookup publishes the inode; a subsequent getattr can
			// watch its own backing directory before granting an attribute TTL.
			for range 2 {
				if _, err := os.Stat(name); err != nil {
					t.Fatal(err)
				}
			}
			before := raw.attrs.Load()
			for range 20 {
				if info, err := os.Stat(name); err != nil || !info.IsDir() {
					t.Fatalf("directory stat: %v %v", info, err)
				}
			}
			want := int64(0)
			if glob == "**/*" {
				want = 20
			}
			if got := raw.attrs.Load() - before; got != want {
				t.Fatalf("directory stats issued %d GETATTRs, want %d", got, want)
			}
		})
	}
}

func TestDirectoryAttributeEvents(t *testing.T) {
	for _, change := range []string{"chmod", "mkdir", "remove", "rename", "replace"} {
		t.Run(change, func(t *testing.T) {
			v, _, _, target, _ := mountedMetadata(t, "notes/**", "directory/child/file.txt")
			name := filepath.Join(target, "directory")
			for range 2 {
				if _, err := os.Stat(name); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch change {
			case "chmod":
				err = v.Project().Chmod("directory", 0700)
			case "mkdir":
				err = v.Project().Mkdir("directory/added", 0755)
			case "remove":
				err = v.Project().RemoveAll("directory/child")
			case "rename":
				err = v.Project().Rename("directory/child", "moved")
			case "replace":
				if err = v.Project().Rename("directory", "moved"); err == nil {
					err = v.Project().Mkdir("directory", 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			want, err := v.Project().Stat("directory")
			if err != nil {
				t.Fatal(err)
			}
			awaitMetadata(t, func() bool {
				info, err := os.Stat(name)
				return err == nil && sameDirectoryAttributes(info, want)
			})
		})
	}
}

func sameDirectoryAttributes(a, b os.FileInfo) bool {
	left, right := a.Sys().(*syscall.Stat_t), b.Sys().(*syscall.Stat_t)
	return left.Mode == right.Mode && left.Nlink == right.Nlink && left.Size == right.Size && left.Mtim == right.Mtim && left.Ctim == right.Ctim
}

func TestDirectoryAttributeInvalidation(t *testing.T) {
	for _, invalidate := range []string{"all", "subtree", "parent", "eviction", "overflow"} {
		t.Run(invalidate, func(t *testing.T) {
			v, root, _, target, _ := mountedMetadata(t, "notes/**", "directory/file.txt")
			name := filepath.Join(target, "directory")
			for range 2 {
				if _, err := os.Stat(name); err != nil {
					t.Fatal(err)
				}
			}
			w := metadataWatcher(v.cache)
			// Lose native events to isolate each invalidation path. The
			// directory has no looked-up children to invalidate incidentally.
			for _, watched := range w.WatchList() {
				if err := w.Remove(watched); err != nil {
					t.Fatal(err)
				}
			}
			// Drop the parent's name record to prove the directory's own watch
			// invalidates attributes even without a tracked entry or child.
			v.cache.mu.Lock()
			delete(v.cache.dirs[&root.Inode].entries, "directory")
			v.cache.mu.Unlock()
			if err := v.Project().Chmod("directory", 0700); err != nil {
				t.Fatal(err)
			}
			switch invalidate {
			case "all":
				v.cache.invalidateAll()
			case "subtree":
				v.cache.invalidatePaths([]string{"directory"})
			case "parent":
				v.cache.invalidatePaths([]string{"directory/file.txt"})
			case "eviction":
				// Root and directory are the only acquired watches.
				v.cache.evictOldest()
				v.cache.evictOldest()
			case "overflow":
				w.Errors <- fsnotify.ErrEventOverflow
				awaitMetadataRecovery(t, v.cache, w)
			}
			if info, err := os.Stat(name); err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("directory attributes survived %s: %v %v", invalidate, info, err)
			}
		})
	}
}

func TestDirectoryAttributeCacheRequiresWatch(t *testing.T) {
	v, root, raw, target, _ := mountedMetadata(t, "notes/**", "directory/file.txt")
	name := filepath.Join(target, "directory")
	for range 2 {
		if _, err := os.Stat(name); err != nil {
			t.Fatal(err)
		}
	}
	v.cache.mu.Lock()
	v.cache.dirs[root.GetChild("directory")].dead = true
	v.cache.mu.Unlock()
	v.cache.invalidateAll()
	if _, err := os.Stat(name); err != nil {
		t.Fatal(err)
	}
	before := raw.attrs.Load()
	for range 3 {
		if _, err := os.Stat(name); err != nil {
			t.Fatal(err)
		}
	}
	if got := raw.attrs.Load() - before; got != 3 {
		t.Fatalf("stats without watch issued %d GETATTRs, want 3", got)
	}
}
