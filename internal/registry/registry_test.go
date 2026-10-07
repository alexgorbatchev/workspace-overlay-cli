package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"workspace-overlay/internal/logged"
	"workspace-overlay/internal/scratch"
)

func TestMain(m *testing.M) {
	scratch.Main(m)
}

func TestRegistryLivesInStateHome(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	root := t.TempDir()
	r, err := Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Record(filepath.Join(root, "project"), "", nil); err != nil {
		t.Fatal(err)
	}
	// Assert r.File() has the prefix filepath.Join(state, "workspace-overlay")
	prefix := filepath.Join(state, "workspace-overlay") + string(filepath.Separator)
	if !filepath.HasPrefix(r.File(), prefix) {
		t.Fatalf("r.File() %q does not have prefix %q", r.File(), prefix)
	}
	// Assert the file exists
	if _, err := os.Stat(r.File()); err != nil {
		t.Fatalf("r.File() does not exist: %v", err)
	}
	// Assert .tmp directory is NOT created under root
	if _, err := os.Stat(filepath.Join(root, ".tmp")); !os.IsNotExist(err) {
		if err == nil {
			t.Fatal(".tmp directory should not exist under root")
		}
		t.Fatalf("unexpected error checking .tmp: %v", err)
	}
	if err := r.Forget(filepath.Join(root, "project")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStateHomeDefaultsToHomeDirectory(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"relative", "relative/state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", tt.value)
			r, err := Open(t.TempDir(), "project")
			if err != nil {
				t.Fatal(err)
			}
			// Assert r.File() has the prefix filepath.Join(home, ".local", "state", "workspace-overlay")
			prefix := filepath.Join(home, ".local", "state", "workspace-overlay") + string(filepath.Separator)
			if !filepath.HasPrefix(r.File(), prefix) {
				t.Fatalf("r.File() %q does not have prefix %q", r.File(), prefix)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSameProjectNameInTwoWorkspaces(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	rootA := t.TempDir()
	rootB := t.TempDir()
	r1, err := Open(rootA, "project")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Open(rootB, "project")
	if err != nil {
		t.Fatal(err)
	}
	if r1.File() == r2.File() {
		t.Fatalf("same project name in different workspaces should have different file paths: %q == %q", r1.File(), r2.File())
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRejectsOtherWorkspace(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	rootA := t.TempDir()
	rootB := t.TempDir()

	// Create a registry for rootA
	r, err := Open(rootA, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Record(filepath.Join(rootA, "project"), "", nil); err != nil {
		t.Fatal(err)
	}

	// The same file claiming another workspace models a key collision.
	foreign := r.state
	foreign.Root = rootB
	data, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	logged.Close(r.lock)
	if err := os.WriteFile(r.File(), data, 0600); err != nil {
		t.Fatal(err)
	}

	// Try to read the registry with rootA should fail
	if _, err := Read(rootA, "project"); err == nil {
		t.Fatal("registry with different root should be rejected")
	}

	// Cleanup
	if err := os.Remove(r.File()); err != nil {
		t.Fatal(err)
	}
}

func TestStateDirCannotDetermineHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	_, err := stateDir()
	if err == nil {
		t.Fatal("expected error when HOME is empty and XDG_STATE_HOME is not set")
	}
	if !strings.Contains(err.Error(), "locate state directory") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestRegistryPathsErrorPropagation(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	_, _, _, err := Paths(t.TempDir(), "project")
	if err == nil {
		t.Fatal("expected error from Paths when stateDir fails")
	}
}

func TestReadRegistryPropagatesStateDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	_, err := Read(t.TempDir(), "project")
	if err == nil {
		t.Fatal("expected error from Read when stateDir fails")
	}
	if !strings.Contains(err.Error(), "locate state directory") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestOpenRegistryErrorInAcquire(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	_, err := Open(t.TempDir(), "project")
	if err == nil {
		t.Fatal("expected error from Open when stateDir fails")
	}
}

func TestRegistryLockAndUpdates(t *testing.T) {
	root := t.TempDir()
	r, err := Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(root, "project"); err == nil {
		other.Close()
		t.Fatal("two owners acquired the project lock")
	}
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	if err := r.Record(b, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(a, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(b, "", []byte("updated")); err != nil {
		t.Fatal(err)
	}
	state, err := Read(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Mounts) != 2 || state.Mounts[0].Target != a || state.Mounts[1].Block != "updated" {
		t.Fatalf("updated registry: %+v", state)
	}
	for _, name := range []string{a, b, "missing"} {
		if err := r.Forget(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if released, err := RequestStop(context.Background(), root, "project"); err != nil || !released {
		t.Fatalf("RequestStop after cleanup: released=%v err=%v", released, err)
	}
}

func TestRegistryValidation(t *testing.T) {
	cases := []struct {
		name  string
		state State
		raw   string
	}{
		{name: "malformed", raw: "{"},
		{name: "version", state: State{Version: 2, Project: "project"}},
		{name: "project", state: State{Version: 1, Project: "other"}},
		{name: "relative target", state: State{Version: 1, Project: "project", Mounts: []Mount{{Target: "relative"}}}},
		{name: "relative exclude", state: State{Version: 1, Project: "project", Mounts: []Mount{{Target: "/absolute", Exclude: "relative"}}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			_, file, _, err := Paths(root, "project")
			if err != nil {
				t.Fatal(err)
			}
			data := tt.raw
			if data == "" {
				encoded, err := json.Marshal(tt.state)
				if err != nil {
					t.Fatal(err)
				}
				data = string(encoded)
			}
			scratch.Write(t, file, data)
			if _, err := Read(root, "project"); err == nil {
				t.Fatal("invalid registry accepted")
			}
			if released, err := RequestStop(context.Background(), root, "project"); err == nil {
				t.Fatalf("invalid stop registry accepted: released=%v err=%v", released, err)
			}
		})
	}
}

func TestRequestStopReportsMissingOwner(t *testing.T) {
	root := t.TempDir()
	r, err := Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Record(filepath.Join(root, "project"), "", nil); err != nil {
		t.Fatal(err)
	}
	// Model a terminated owner by unlocking without cleaning up
	r.Unlock()

	// An owner that is gone holds no lock, so there is nothing to wait for and
	// nobody to leave a stop request for.
	start := time.Now()
	released, err := RequestStop(context.Background(), root, "project")
	if released || err != nil {
		t.Fatalf("RequestStop with missing owner: released=%v err=%v, want false, nil", released, err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %s for an owner that is gone", waited)
	}
	if _, err := os.Stat(r.StopFile()); !os.IsNotExist(err) {
		t.Fatalf("stop request left for a missing owner: %v", err)
	}

	// Cleanup so tests don't leave state
	r2, err := Acquire(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	r2.Adopt(State{Version: 1, Root: root, Project: "project", Mounts: []Mount{{Target: filepath.Join(root, "project")}}})
	if err := r2.Forget(filepath.Join(root, "project")); err != nil {
		t.Fatal(err)
	}
	if err := r2.Discard(); err != nil {
		t.Fatal(err)
	}
	r2.Unlock()
}

func TestRegistryNativeFailures(t *testing.T) {
	t.Run("unreadable registry", func(t *testing.T) {
		root := t.TempDir()
		_, file, _, err := Paths(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(file, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(root, "project"); err == nil {
			t.Fatal("registry directory accepted")
		}
	})
	t.Run("blocked state directory", func(t *testing.T) {
		root := t.TempDir()
		// Block XDG_STATE_HOME by pointing it to a regular file
		blockFile := filepath.Join(root, "block-state")
		if err := os.WriteFile(blockFile, []byte("blocking"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_STATE_HOME", blockFile)
		if r, err := Open(root, "project"); err == nil {
			r.Close()
			t.Fatal("blocked state directory accepted")
		}
	})
	t.Run("blocked lock", func(t *testing.T) {
		root := t.TempDir()
		_, file, _, err := Paths(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(file+".lock", 0755); err != nil {
			t.Fatal(err)
		}
		if r, err := Open(root, "project"); err == nil {
			r.Close()
			t.Fatal("blocked lock accepted")
		}
	})
	t.Run("blocked atomic save", func(t *testing.T) {
		root := t.TempDir()
		r, err := Open(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		defer logged.Close(r.lock)
		if err := os.Mkdir(r.File()+".next", 0755); err != nil {
			t.Fatal(err)
		}
		if err := r.Record(filepath.Join(root, "project"), "", nil); err == nil {
			t.Fatal("blocked atomic registry write accepted")
		}
	})
	t.Run("canceled stop", func(t *testing.T) {
		root := t.TempDir()
		r, err := Open(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "project")
		if err := r.Record(target, "", nil); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if released, err := RequestStop(ctx, root, "project"); err == nil {
			t.Fatalf("canceled stop accepted: released=%v", released)
		}
		// A request that was canceled before it started must not stop the owner.
		if _, err := os.Stat(r.StopFile()); !os.IsNotExist(err) {
			t.Fatalf("canceled request left a stop file behind: %v", err)
		}
		if err := r.Forget(target); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
