package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureCreate(t *testing.T) {
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	root := filepath.Join(t.TempDir(), "workspace")
	os.Args = []string{"workspace-overlay", "fixture", "create", "--directory", root}
	var err error
	out, _ := captureOutput(t, func() { err = run() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, displayPath(filepath.Join(root, configName))) {
		t.Fatalf("configuration output: %q", out)
	}
	config, err := loadConfig(filepath.Join(root, configName))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		project := config.Projects[name]
		worktrees, err := discoverWorktrees(t.Context(), project.Path, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(worktrees) != 2 {
			t.Fatalf("%s worktrees: %v", name, worktrees)
		}
		out, err := projectGit(t.Context(), project.Path, "status", "--porcelain").CombinedOutput()
		if err != nil || len(out) != 0 {
			t.Fatalf("fixture backing files are not committed: %s (%v)", out, err)
		}
		out, err = projectGit(t.Context(), project.Path, "branch", "--show-current").CombinedOutput()
		if err != nil || string(out) != "main\n" {
			t.Fatalf("fixture branch: %s (%v)", out, err)
		}
	}
	edit := filepath.Join(root, ".ai", "alpha", "AGENTS.md")
	writeFixture(t, edit, "retained edit\n")
	captureOutput(t, func() { err = run() })
	if err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadFile(t, edit)); got != "retained edit\n" {
		t.Fatalf("restart overwrote edit: %q", got)
	}
}

func TestFixtureFailures(t *testing.T) {
	t.Run("parent is a file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "file")
		writeFixture(t, parent, "keep")
		if _, err := createFixture(t.Context(), filepath.Join(parent, "workspace")); err == nil {
			t.Fatal("accepted file as parent")
		}
	})
	t.Run("broken completed configuration", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "workspace")
		config, err := createFixture(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, config, "invalid TOML [")
		if _, err := createFixture(t.Context(), root); err == nil {
			t.Fatal("reused invalid configuration")
		}
	})
	t.Run("cancelled initialization retains incomplete data", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		root := filepath.Join(t.TempDir(), "workspace")
		if _, err := createFixture(ctx, root); err == nil {
			t.Fatal("ignored cancellation")
		}
		mustReadFile(t, filepath.Join(root, "alpha", "AGENTS.md"))
		if _, err := createFixture(t.Context(), root); err == nil {
			t.Fatal("overwrote incomplete fixture")
		}
	})
	t.Run("copy destination is a file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "file")
		writeFixture(t, root, "keep")
		if err := copyFixture(root); err == nil {
			t.Fatal("copied onto file")
		}
	})
}

func TestFixtureGitIgnoresInheritedRepositoryOverrides(t *testing.T) {
	foreign := t.TempDir()
	writeFixture(t, filepath.Join(foreign, "keep.txt"), "keep")
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign)
	root := filepath.Join(t.TempDir(), "workspace")
	if _, err := createFixture(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(foreign, ".git")); !os.IsNotExist(err) {
		t.Fatalf("foreign Git directory changed: %v", err)
	}
	if got := string(mustReadFile(t, filepath.Join(foreign, "keep.txt"))); got != "keep" {
		t.Fatalf("foreign data changed: %q", got)
	}
}

func TestFixtureCreateRefusesForeignDirectory(t *testing.T) {
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "keep.txt"), "keep")
	os.Args = []string{"workspace-overlay", "fixture", "create", "--directory", root}
	var err error
	captureOutput(t, func() { err = run() })
	if err == nil {
		t.Fatal("accepted foreign directory")
	}
	if got := string(mustReadFile(t, filepath.Join(root, "keep.txt"))); got != "keep" {
		t.Fatalf("foreign data changed: %q", got)
	}
}
