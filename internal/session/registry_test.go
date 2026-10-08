package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitexclude"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/registry"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
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
		t.Fatalf("stale registry overwritten without explicit recovery (closing it: %v)", next.Close())
	}
	if err := recoverRegistry(context.Background(), root, "project", nil); err != nil {
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
	if err := recoverRegistry(context.Background(), root, "project", nil); err != nil {
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
	if err := recoverRegistry(context.Background(), root, "project", nil); err == nil {
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
	if err := stopRegistered(context.Background(), root, "project", nil); err != nil {
		t.Fatal(err)
	}
	state, err := registry.Read(root, "project")
	if err != nil || state.Version != 0 {
		t.Fatalf("terminated owner not recovered: %+v %v", state, err)
	}
}

func TestRecoveryRefusesLiveOwner(t *testing.T) {
	root := t.TempDir()
	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := recoverRegistry(context.Background(), root, "project", nil); err == nil {
		t.Fatal("recovery took over live owner")
	}
}
