package registry

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
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

func TestLinkedFollowsTheFileUnderItsName(t *testing.T) {
	name := filepath.Join(t.TempDir(), "lock")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})

	steps := []struct {
		name   string
		change func() error
		want   bool
	}{
		{"untouched", func() error { return nil }, true},
		{"removed", func() error { return os.Remove(name) }, false},
		{"replaced", func() error { return os.WriteFile(name, nil, 0600) }, false},
	}
	for _, step := range steps {
		if err := step.change(); err != nil {
			t.Fatal(err)
		}
		if got, err := linked(file); err != nil || got != step.want {
			t.Fatalf("%s: linked = %v, %v; want %v", step.name, got, err, step.want)
		}
	}
}

func TestUnlockReportsFailedRelease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	r, err := Acquire(t.TempDir(), "project")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	// Closing the handle behind the registry's back makes the release fail.
	if err := r.lock.Close(); err != nil {
		t.Fatal(err)
	}
	r.Unlock()

	if !strings.Contains(logs.String(), "release project lock") {
		t.Fatalf("failed release was not reported: %q", logs.String())
	}
	if _, err := os.Stat(r.File() + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock file after a failed release: %v, want it removed", err)
	}
}

func TestOpenReleasesLockWhenStopRequestCannotBeCleared(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	_, file, stop, err := Paths(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	// A directory with content cannot be removed the way a stop file is.
	scratch.Write(t, filepath.Join(stop, "blocker"), "")

	if r, err := Open(root, "project"); err == nil {
		r.Unlock()
		t.Fatal("leftover stop request that cannot be cleared was accepted")
	}
	if _, err := os.Stat(file + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock file after a refused open: %v, want it removed", err)
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
