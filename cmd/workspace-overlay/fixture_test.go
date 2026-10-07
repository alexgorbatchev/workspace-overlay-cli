package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"workspace-overlay/internal/config"
	"workspace-overlay/internal/gitrepo"
	"workspace-overlay/internal/pathname"
	"workspace-overlay/internal/scratch"
)

func TestFixtureCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := run([]string{"fixture", "create", "--directory", root}, stdout, stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), pathname.Display(filepath.Join(root, config.Name))) {
		t.Fatalf("configuration output: %q", stdout.String())
	}
	cfg, err := config.Load(filepath.Join(root, config.Name))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		project := cfg.Projects[name]
		listed, err := gitrepo.Command(t.Context(), project.Path, "worktree", "list", "--porcelain").Output()
		if err != nil {
			t.Fatal(err)
		}
		if worktrees := strings.Count(string(listed), "worktree "); worktrees != 2 {
			t.Fatalf("%s worktrees: %d in %q", name, worktrees, listed)
		}
		out, err := gitrepo.Command(t.Context(), project.Path, "status", "--porcelain").CombinedOutput()
		if err != nil || len(out) != 0 {
			t.Fatalf("fixture backing files are not committed: %s (%v)", out, err)
		}
		out, err = gitrepo.Command(t.Context(), project.Path, "branch", "--show-current").CombinedOutput()
		if err != nil || string(out) != "main\n" {
			t.Fatalf("fixture branch: %s (%v)", out, err)
		}
	}
	edit := filepath.Join(root, ".ai", "alpha", "AGENTS.md")
	scratch.Write(t, edit, "retained edit\n")
	stdout = new(bytes.Buffer)
	stderr = new(bytes.Buffer)
	err = run([]string{"fixture", "create", "--directory", root}, stdout, stderr)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(scratch.Read(t, edit)); got != "retained edit\n" {
		t.Fatalf("restart overwrote edit: %q", got)
	}
}

func TestFixtureCreateRefusesForeignDirectory(t *testing.T) {
	root := t.TempDir()
	scratch.Write(t, filepath.Join(root, "keep.txt"), "keep")
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := run([]string{"fixture", "create", "--directory", root}, stdout, stderr)
	if err == nil {
		t.Fatal("accepted foreign directory")
	}
	if got := string(scratch.Read(t, filepath.Join(root, "keep.txt"))); got != "keep" {
		t.Fatalf("foreign data changed: %q", got)
	}
}
