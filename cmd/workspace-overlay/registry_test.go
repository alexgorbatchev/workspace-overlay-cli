package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryPreservesUnfinishedCleanup(t *testing.T) {
	root := t.TempDir()
	r, err := openRegistry(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.record(filepath.Join(root, "project"), "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.close(); err == nil {
		t.Fatal("unfinished cleanup should retain a recoverable registry and report an error")
	}
	state, err := readRegistry(root, "project")
	if err != nil || len(state.Mounts) != 1 {
		t.Fatalf("unfinished mount record lost: %+v, %v", state, err)
	}
	if next, err := openRegistry(root, "project"); err == nil {
		next.close()
		t.Fatal("stale registry overwritten without explicit recovery")
	}
	if err := recoverRegistry(context.Background(), root, "project"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRecovery(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	initGitRepoWithCommit(t, project)
	exclude := filepath.Join(project, ".git", "info", "exclude")
	original, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openRegistry(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	g, err := openExclude(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	g.onUpdate = func(block []byte) error { return r.record(project, g.file, block) }
	if err := g.update([]string{"/generated.md"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, append(mustReadFile(t, exclude), []byte("personal-pattern\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	// Release native handles to model a terminated owner without its cleanup.
	if err := g.root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recoverRegistry(context.Background(), root, "project"); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadFile(t, exclude)); got != string(original)+"personal-pattern\n" {
		t.Fatalf("recovery damaged user exclusions: %q", got)
	}
	state, err := readRegistry(root, "project")
	if err != nil || len(state.Mounts) != 0 {
		t.Fatalf("registry not cleared: %+v, %v", state, err)
	}
}

func mustReadFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRegistryValidation(t *testing.T) {
	cases := []struct {
		name  string
		state mountState
		raw   string
	}{
		{name: "malformed", raw: "{"},
		{name: "version", state: mountState{Version: 2, Project: "project"}},
		{name: "project", state: mountState{Version: 1, Project: "other"}},
		{name: "relative target", state: mountState{Version: 1, Project: "project", Mounts: []recordedMount{{Target: "relative"}}}},
		{name: "relative exclude", state: mountState{Version: 1, Project: "project", Mounts: []recordedMount{{Target: "/absolute", Exclude: "relative"}}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			_, file, _ := registryPaths(root, "project")
			data := tt.raw
			if data == "" {
				encoded, err := json.Marshal(tt.state)
				if err != nil {
					t.Fatal(err)
				}
				data = string(encoded)
			}
			writeFixture(t, file, data)
			if _, err := readRegistry(root, "project"); err == nil {
				t.Fatal("invalid registry accepted")
			}
			if err := stopRegistered(context.Background(), root, "project"); err == nil {
				t.Fatal("invalid stop registry accepted")
			}
		})
	}
}

func TestRegistryLockAndUpdates(t *testing.T) {
	root := t.TempDir()
	r, err := openRegistry(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if other, err := openRegistry(root, "project"); err == nil {
		other.close()
		t.Fatal("two owners acquired the project lock")
	}
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	if err := r.record(b, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.record(a, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.record(b, "", []byte("updated")); err != nil {
		t.Fatal(err)
	}
	state, err := readRegistry(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Mounts) != 2 || state.Mounts[0].Target != a || state.Mounts[1].Block != "updated" {
		t.Fatalf("updated registry: %+v", state)
	}
	for _, name := range []string{a, b, "missing"} {
		if err := r.forget(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	if err := stopRegistered(context.Background(), root, "project"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRecoveryRetainsChangedBlock(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	initGitRepoWithCommit(t, project)
	r, err := openRegistry(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	g, err := openExclude(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	g.onUpdate = func(block []byte) error { return r.record(project, g.file, block) }
	if err := g.update([]string{"/generated.md"}); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, g.file, strings.Replace(string(mustReadFile(t, g.file)), "/generated.md", "/edited.md", 1))
	if err := g.root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recoverRegistry(context.Background(), root, "project"); err == nil {
		t.Fatal("changed managed block overwritten")
	}
	state, err := readRegistry(root, "project")
	if err != nil || len(state.Mounts) != 1 {
		t.Fatalf("recovery evidence lost: %+v %v", state, err)
	}
}

func TestStopRecoversTerminatedOwner(t *testing.T) {
	root := t.TempDir()
	r, err := openRegistry(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.record(filepath.Join(root, "project"), "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stopRegistered(context.Background(), root, "project"); err != nil {
		t.Fatal(err)
	}
	state, err := readRegistry(root, "project")
	if err != nil || state.Version != 0 {
		t.Fatalf("terminated owner not recovered: %+v %v", state, err)
	}
}

func TestRegistryNativeFailures(t *testing.T) {
	t.Run("unreadable registry", func(t *testing.T) {
		root := t.TempDir()
		_, file, _ := registryPaths(root, "project")
		if err := os.MkdirAll(file, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := readRegistry(root, "project"); err == nil {
			t.Fatal("registry directory accepted")
		}
	})
	t.Run("blocked state directory", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, filepath.Join(root, ".tmp"), "file")
		if r, err := openRegistry(root, "project"); err == nil {
			r.close()
			t.Fatal("blocked state directory accepted")
		}
	})
	t.Run("blocked lock", func(t *testing.T) {
		root := t.TempDir()
		_, file, _ := registryPaths(root, "project")
		if err := os.MkdirAll(file+".lock", 0755); err != nil {
			t.Fatal(err)
		}
		if r, err := openRegistry(root, "project"); err == nil {
			r.close()
			t.Fatal("blocked lock accepted")
		}
	})
	t.Run("blocked atomic save", func(t *testing.T) {
		root := t.TempDir()
		r, err := openRegistry(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		defer closeFile(r.lock)
		if err := os.Mkdir(r.file+".next", 0755); err != nil {
			t.Fatal(err)
		}
		if err := r.record(filepath.Join(root, "project"), "", nil); err == nil {
			t.Fatal("blocked atomic registry write accepted")
		}
	})
	t.Run("canceled stop", func(t *testing.T) {
		root := t.TempDir()
		r, err := openRegistry(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "project")
		if err := r.record(target, "", nil); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := stopRegistered(ctx, root, "project"); err == nil {
			t.Fatal("canceled stop accepted")
		}
		if err := r.forget(target); err != nil {
			t.Fatal(err)
		}
		if err := r.close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("live owner recovery refused", func(t *testing.T) {
		root := t.TempDir()
		r, err := openRegistry(root, "project")
		if err != nil {
			t.Fatal(err)
		}
		defer r.close()
		if err := recoverRegistry(context.Background(), root, "project"); err == nil {
			t.Fatal("recovery took over live owner")
		}
	})
}
