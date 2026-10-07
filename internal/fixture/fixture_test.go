package fixture

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMain(m *testing.M) { scratch.Main(m) }

func TestFixtureFailures(t *testing.T) {
	t.Run("parent is a file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "file")
		scratch.Write(t, parent, "keep")
		if _, err := Create(t.Context(), filepath.Join(parent, "workspace")); err == nil {
			t.Fatal("accepted file as parent")
		}
	})
	t.Run("broken completed configuration", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "workspace")
		configFile, err := Create(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		scratch.Write(t, configFile, "invalid TOML [")
		if _, err := Create(t.Context(), root); err == nil {
			t.Fatal("reused invalid configuration")
		}
	})
	t.Run("cancelled initialization retains incomplete data", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		root := filepath.Join(t.TempDir(), "workspace")
		if _, err := Create(ctx, root); err == nil {
			t.Fatal("ignored cancellation")
		}
		scratch.Read(t, filepath.Join(root, "alpha", "AGENTS.md"))
		if _, err := Create(t.Context(), root); err == nil {
			t.Fatal("overwrote incomplete fixture")
		}
	})
	t.Run("copy destination is a file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "file")
		scratch.Write(t, root, "keep")
		if err := populate(root); err == nil {
			t.Fatal("copied onto file")
		}
	})
}

func TestFixtureGitIgnoresInheritedRepositoryOverrides(t *testing.T) {
	foreign := t.TempDir()
	scratch.Write(t, filepath.Join(foreign, "keep.txt"), "keep")
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign)
	root := filepath.Join(t.TempDir(), "workspace")
	if _, err := Create(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(foreign, ".git")); !os.IsNotExist(err) {
		t.Fatalf("foreign Git directory changed: %v", err)
	}
	if got := string(scratch.Read(t, filepath.Join(foreign, "keep.txt"))); got != "keep" {
		t.Fatalf("foreign data changed: %q", got)
	}
}
