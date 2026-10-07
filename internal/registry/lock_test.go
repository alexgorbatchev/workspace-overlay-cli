package registry

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func stateEntries(t *testing.T, r *Registry) []string {
	t.Helper()
	entries, err := os.ReadDir(r.Dir())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}

func TestStateDirectoryIsEmptyAfterCleanShutdown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	r, err := Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "project")
	if err := r.Record(target, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Forget(target); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if names := stateEntries(t, r); len(names) != 0 {
		t.Fatalf("state directory after a clean shutdown holds %v, want nothing", names)
	}
}

func TestLockFileIsRemovedOnRelease(t *testing.T) {
	tests := []struct {
		name    string
		release func(t *testing.T, r *Registry)
	}{
		{"close", func(t *testing.T, r *Registry) {
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"unlock", func(t *testing.T, r *Registry) { r.Unlock() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			r, err := Acquire(t.TempDir(), "project")
			if err != nil {
				t.Fatal(err)
			}
			lock := r.File() + ".lock"
			if _, err := os.Stat(lock); err != nil {
				t.Fatalf("lock file while held: %v", err)
			}
			tt.release(t, r)
			if _, err := os.Stat(lock); !os.IsNotExist(err) {
				t.Fatalf("lock file after release: %v, want it removed", err)
			}
		})
	}
}

// The record of an owner that died must stay for recovery, but an Open that
// is refused because of it must not add a lock file of its own.
func TestRefusedOpenKeepsOnlyTheRecord(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	r, err := Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Record(filepath.Join(root, "project"), "", nil); err != nil {
		t.Fatal(err)
	}
	r.Unlock()
	if _, err := Open(root, "project"); err == nil {
		t.Fatal("unfinished registry accepted")
	}
	if names := stateEntries(t, r); len(names) != 1 || names[0] != filepath.Base(r.File()) {
		t.Fatalf("state directory holds %v, want only the record", names)
	}
}

func TestBusyLockSurvivesContender(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	r, err := Acquire(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Unlock()
	// The second attempt proves the first refusal left the lock in force.
	for range 2 {
		if _, err := Acquire(root, "project"); !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatalf("contender got %v, want the project reported busy", err)
		}
		if _, err := os.Stat(r.File() + ".lock"); err != nil {
			t.Fatalf("owner's lock file after a refused contender: %v", err)
		}
	}
}

// Removing the lock file on release must never let two owners in at once: a
// contender that opened the file just before its removal would otherwise hold
// a lock nobody else can see.
func TestLockStaysExclusiveWhileOwnersComeAndGo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	var holders, overlaps atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 300 {
				r, err := Acquire(root, "project")
				if errors.Is(err, syscall.EWOULDBLOCK) {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				if holders.Add(1) > 1 {
					overlaps.Add(1)
				}
				runtime.Gosched()
				holders.Add(-1)
				r.Unlock()
			}
		})
	}
	wg.Wait()
	if n := overlaps.Load(); n != 0 {
		t.Fatalf("two owners held the project lock at once %d times", n)
	}
}
