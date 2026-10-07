package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"workspace-overlay/internal/gitexclude"
	"workspace-overlay/internal/registry"
	"workspace-overlay/internal/scratch"
)

func TestRegistryPreservesUnfinishedCleanup(t *testing.T) {
	root := t.TempDir()
	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Record(filepath.Join(root, "project"), "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err == nil {
		t.Fatal("unfinished cleanup should retain a recoverable registry and report an error")
	}
	state, err := registry.Read(root, "project")
	if err != nil || len(state.Mounts) != 1 {
		t.Fatalf("unfinished mount record lost: %+v, %v", state, err)
	}
	if next, err := registry.Open(root, "project"); err == nil {
		next.Close()
		t.Fatal("stale registry overwritten without explicit recovery")
	}
	if err := recoverRegistry(context.Background(), root, "project"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRecovery(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	exclude := filepath.Join(project, ".git", "info", "exclude")
	original, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	g, err := gitexclude.Open(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	g.OnUpdate = func(block []byte) error { return r.Record(project, g.Path(), block) }
	if err := g.Update([]string{"/generated.md"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, append(scratch.Read(t, exclude), []byte("personal-pattern\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	// Model a terminated owner by unlocking without cleaning up (not calling Close).
	r.Unlock()
	if err := recoverRegistry(context.Background(), root, "project"); err != nil {
		t.Fatal(err)
	}
	if got := string(scratch.Read(t, exclude)); got != string(original)+"personal-pattern\n" {
		t.Fatalf("recovery damaged user exclusions: %q", got)
	}
	state, err := registry.Read(root, "project")
	if err != nil || len(state.Mounts) != 0 {
		t.Fatalf("registry not cleared: %+v, %v", state, err)
	}
}

func TestRegistryValidation(t *testing.T) {
	cases := []struct {
		name  string
		state registry.State
		raw   string
	}{
		{name: "malformed", raw: "{"},
		{name: "version", state: registry.State{Version: 2, Project: "project"}},
		{name: "project", state: registry.State{Version: 1, Project: "other"}},
		{name: "relative target", state: registry.State{Version: 1, Project: "project", Mounts: []registry.Mount{{Target: "relative"}}}},
		{name: "relative exclude", state: registry.State{Version: 1, Project: "project", Mounts: []registry.Mount{{Target: "/absolute", Exclude: "relative"}}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			_, file, _, err := registry.Paths(root, "project")
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
			if _, err := registry.Read(root, "project"); err == nil {
				t.Fatal("invalid registry accepted")
			}
			released, err := registry.RequestStop(context.Background(), root, "project")
			if err == nil && !released {
				t.Fatal("invalid stop registry accepted")
			}
		})
	}
}

func TestRegistryLockAndUpdates(t *testing.T) {
	root := t.TempDir()
	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if other, err := registry.Open(root, "project"); err == nil {
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
	state, err := registry.Read(root, "project")
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
	released, err := registry.RequestStop(context.Background(), root, "project")
	if err != nil || !released {
		t.Fatalf("RequestStop: released=%v err=%v", released, err)
	}
}

func TestRegistryRecoveryRetainsChangedBlock(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	g, err := gitexclude.Open(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	g.OnUpdate = func(block []byte) error { return r.Record(project, g.Path(), block) }
	if err := g.Update([]string{"/generated.md"}); err != nil {
		t.Fatal(err)
	}
	scratch.Write(t, g.Path(), strings.Replace(string(scratch.Read(t, g.Path())), "/generated.md", "/edited.md", 1))
	// Model a terminated owner by unlocking without closing
	r.Unlock()
	if err := recoverRegistry(context.Background(), root, "project"); err == nil {
		t.Fatal("changed managed block overwritten")
	}
	state, err := registry.Read(root, "project")
	if err != nil || len(state.Mounts) != 1 {
		t.Fatalf("recovery evidence lost: %+v %v", state, err)
	}
}

func TestStopRecoversTerminatedOwner(t *testing.T) {
	root := t.TempDir()
	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Record(filepath.Join(root, "project"), "", nil); err != nil {
		t.Fatal(err)
	}
	// Model a terminated owner by unlocking
	r.Unlock()
	if err := stopRegistered(context.Background(), root, "project"); err != nil {
		t.Fatal(err)
	}
	state, err := registry.Read(root, "project")
	if err != nil || state.Version != 0 {
		t.Fatalf("terminated owner not recovered: %+v %v", state, err)
	}
}

func TestRegistryNativeFailures(t *testing.T) {
	t.Run("unreadable registry", func(t *testing.T) {
		root := t.TempDir()
		_, file, _, err := registry.Paths(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(file, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Read(root, "project"); err == nil {
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
		if r, err := registry.Open(root, "project"); err == nil {
			r.Close()
			t.Fatal("blocked state directory accepted")
		}
	})
	t.Run("blocked lock", func(t *testing.T) {
		root := t.TempDir()
		_, file, _, err := registry.Paths(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(file+".lock", 0755); err != nil {
			t.Fatal(err)
		}
		if r, err := registry.Open(root, "project"); err == nil {
			r.Close()
			t.Fatal("blocked lock accepted")
		}
	})
	t.Run("blocked atomic save", func(t *testing.T) {
		root := t.TempDir()
		r, err := registry.Open(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Unlock()
		if err := os.Mkdir(r.File()+".next", 0755); err != nil {
			t.Fatal(err)
		}
		if err := r.Record(filepath.Join(root, "project"), "", nil); err == nil {
			t.Fatal("blocked atomic registry write accepted")
		}
	})
	t.Run("canceled stop", func(t *testing.T) {
		root := t.TempDir()
		r, err := registry.Open(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "project")
		if err := r.Record(target, "", nil); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if released, err := registry.RequestStop(ctx, root, "project"); err == nil {
			t.Fatalf("canceled RequestStop accepted: released=%v", released)
		}
		if err := r.Forget(target); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRecoveryRefusesLiveOwner(t *testing.T) {
	root := t.TempDir()
	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := recoverRegistry(context.Background(), root, "project"); err == nil {
		t.Fatal("recovery took over live owner")
	}
}
