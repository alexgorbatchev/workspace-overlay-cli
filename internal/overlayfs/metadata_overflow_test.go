package overlayfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestMetadataOverflowRecovers(t *testing.T) {
	v, root, raw, target, _ := mountedMetadata(t, "notes/**", "file.txt")
	readNative(t, filepath.Join(target, "file.txt"), "base")
	before := raw.reads.Load()
	// Deliver the real overflow sentinel at the same seam as inotify. It
	// must discard existing pages, then allow caching again without remounting.
	w := metadataWatcher(v.cache)
	w.Errors <- fsnotify.ErrEventOverflow
	awaitMetadataRecovery(t, v.cache, w)
	readNative(t, filepath.Join(target, "file.txt"), "base")
	if raw.reads.Load() <= before {
		t.Fatal("overflow did not invalidate cached pages")
	}
	var out fuse.EntryOut
	if _, errno := root.Lookup(context.Background(), "file.txt", &out); errno != 0 {
		t.Fatal(errno)
	}
	if out.EntryTimeout() == 0 || out.AttrTimeout() == 0 {
		t.Fatal("one event overflow permanently disabled metadata caching")
	}
}

func metadataWatcher(c *metadataCache) *fsnotify.Watcher {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watcher
}

func awaitMetadataRecovery(t *testing.T, c *metadataCache, old *fsnotify.Watcher) {
	t.Helper()
	awaitMetadata(t, func() bool {
		return metadataWatcher(c) != old && !c.inbox.paused.Load()
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		t.Fatal("overflow permanently disabled caching")
	}
}

func TestMetadataOverflowRestoresHeldFilesAndMisses(t *testing.T) {
	for _, glob := range []string{"notes/**", "**/*"} {
		t.Run(glob, func(t *testing.T) {
			v, _, _, target, _ := mountedMetadata(t, glob, "file.txt")
			f, err := os.Open(filepath.Join(target, "file.txt"))
			if err != nil {
				t.Fatal(err)
			}
			defer closeNativeTestFile(t, f)
			readMetadataHandle(t, f, "base")
			missing := filepath.Join(target, "missing.txt")
			if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			w := metadataWatcher(v.cache)
			// Remove native watches to reproduce actual event loss, including
			// lost watch bookkeeping, without changing host inotify limits.
			for _, name := range w.WatchList() {
				if err := w.Remove(name); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"file.txt", "missing.txt"} {
				if err := v.Project().WriteFile(name, []byte("edit"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			w.Errors <- fsnotify.ErrEventOverflow
			awaitMetadataRecovery(t, v.cache, w)
			readMetadataHandle(t, f, "edit")
			readNative(t, missing, "edit")
			// No reopen of f: the rebuilt watch must cover subsequent writes
			// to pages retained by an already-open descriptor.
			if err := v.Project().WriteFile("file.txt", []byte("live"), 0644); err != nil {
				t.Fatal(err)
			}
			awaitMetadata(t, func() bool {
				data := make([]byte, 4)
				_, err := f.ReadAt(data, 0)
				return err == nil && string(data) == "live"
			})
		})
	}
}

func readMetadataHandle(t *testing.T, f *os.File, want string) {
	t.Helper()
	data := make([]byte, len(want))
	if _, err := f.ReadAt(data, 0); err != nil || string(data) != want {
		t.Fatalf("held descriptor: %q %v, want %q", data, err, want)
	}
}

func TestMetadataEventBacklogCoalescesAndRecovers(t *testing.T) {
	v, root, _, target, _ := mountedMetadata(t, "notes/**", "file.txt")
	readNative(t, filepath.Join(target, "file.txt"), "base")
	c := v.cache
	w := metadataWatcher(c)
	event := metadataEvent(v, root, "file.txt", fsnotify.Write)
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.flush <- struct{}{}
		awaitMetadata(t, func() bool { return len(c.flush) == 0 })
		for range metadataEventLimit * 2 {
			w.Events <- event
		}
		// Synchronize with intake of the last write by delivering its chmod.
		w.Events <- fsnotify.Event{Name: event.Name, Op: fsnotify.Chmod}
		awaitMetadata(t, func() bool {
			c.inbox.mu.Lock()
			defer c.inbox.mu.Unlock()
			return len(c.inbox.events) == 1 && c.inbox.events[event.Name] == fsnotify.Write|fsnotify.Chmod
		})
		for i := range metadataEventLimit + 1 {
			w.Events <- fsnotify.Event{Name: fmt.Sprintf("%s-%d", event.Name, i), Op: fsnotify.Create}
		}
		awaitMetadata(t, func() bool { return c.inbox.paused.Load() })
		c.inbox.mu.Lock()
		defer c.inbox.mu.Unlock()
		if !c.inbox.lost || len(c.inbox.events) != 0 {
			t.Fatal("full event backlog did not replace queued work with recovery")
		}
	}()
	awaitMetadataRecovery(t, c, w)
}

func TestMetadataNativeWriteBurst(t *testing.T) {
	v, root, _, target, _ := mountedMetadata(t, "notes/**", "a.txt", "b.txt", "marker.txt")
	for _, name := range []string{"a.txt", "b.txt"} {
		readNative(t, filepath.Join(target, name), "base")
	}
	a, err := v.Project().OpenFile("a.txt", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer closeNativeTestFile(t, a)
	b, err := v.Project().OpenFile("b.txt", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer closeNativeTestFile(t, b)
	marker, err := os.Open(filepath.Join(target, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeNativeTestFile(t, marker)
	readMetadataHandle(t, marker, "base")
	func() {
		v.cache.mu.Lock()
		defer v.cache.mu.Unlock()
		// Alternate files so inotify cannot coalesce consecutive identical
		// events. More writes than this host's normal kernel queue can hold.
		for range 20000 {
			for _, f := range []*os.File{a, b} {
				if _, err := f.WriteAt([]byte("edit"), 0); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := v.Project().WriteFile("marker.txt", []byte("done"), 0644); err != nil {
			t.Fatal(err)
		}
	}()
	// Observing the final write through a held descriptor proves native
	// intake reached the burst's tail (or recovery flushed the lost write).
	awaitMetadata(t, func() bool {
		data := make([]byte, 4)
		_, err := marker.ReadAt(data, 0)
		return err == nil && string(data) == "done"
	})
	awaitMetadata(t, func() bool {
		var out fuse.EntryOut
		_, errno := root.Lookup(context.Background(), "a.txt", &out)
		return errno == 0 && out.AttrTimeout() > 0
	})
	readNative(t, filepath.Join(target, "a.txt"), "edit")
	readNative(t, filepath.Join(target, "b.txt"), "edit")
}

func TestMetadataOverflowDetectsMissedDirectoryRename(t *testing.T) {
	v, root, _, target, _ := mountedMetadata(t, "notes/**", "dir/file.txt")
	f, err := os.Open(filepath.Join(target, "dir/file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeNativeTestFile(t, f)
	readMetadataHandle(t, f, "base")
	oldDir := root.GetChild("dir").Operations().(*node)
	w := metadataWatcher(v.cache)
	for _, name := range w.WatchList() {
		if err := w.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Project().Rename("dir", "moved"); err != nil {
		t.Fatal(err)
	}
	if err := v.Project().Mkdir("dir", 0755); err != nil {
		t.Fatal(err)
	}
	if err := v.Project().WriteFile("dir/file.txt", []byte("new!"), 0644); err != nil {
		t.Fatal(err)
	}
	w.Errors <- fsnotify.ErrEventOverflow
	awaitMetadataRecovery(t, v.cache, w)
	if v.cache.watch(oldDir, "file.txt") {
		t.Fatal("missed rename reused a watch for a different backing directory")
	}
	readNative(t, filepath.Join(target, "dir/file.txt"), "new!")
	if err := v.Project().WriteFile("moved/file.txt", []byte("live"), 0644); err != nil {
		t.Fatal(err)
	}
	awaitMetadata(t, func() bool {
		data := make([]byte, 4)
		_, err := f.ReadAt(data, 0)
		return err == nil && string(data) == "live"
	})
}

func TestMetadataOverflowAfterWatchedDirectoryRemoval(t *testing.T) {
	v, _, _, target, _ := mountedMetadata(t, "notes/**", "dir/file.txt")
	readNative(t, filepath.Join(target, "dir/file.txt"), "base")
	w := metadataWatcher(v.cache)
	for _, name := range w.WatchList() {
		if err := w.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"dir/file.txt", "dir"} {
		if err := v.Project().Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	w.Errors <- fsnotify.ErrEventOverflow
	awaitMetadataRecovery(t, v.cache, w)
	if _, err := os.Stat(filepath.Join(target, "dir/file.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed directory survived recovery: %v", err)
	}
}

func TestMetadataEventsDrainDuringInvalidation(t *testing.T) {
	v, root, _, target, _ := mountedMetadata(t, "notes/**", "file.txt")
	readNative(t, filepath.Join(target, "file.txt"), "base")
	event := metadataEvent(v, root, "file.txt", fsnotify.Write)
	c := v.cache
	// Hold the invalidation worker at its cache lock. Event intake must
	// continue independently, as it must during slow kernel notifications.
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for range 4096 {
		select {
		case c.watcher.Events <- event:
		case <-timer.C:
			t.Fatal("cache invalidation blocked filesystem event intake")
		}
	}
}
