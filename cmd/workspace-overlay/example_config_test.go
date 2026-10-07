package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExampleConfigurationMountsAndWrites(t *testing.T) {
	const shutdownTimeout = 5 * time.Second
	template := mustReadFile(t, filepath.Join("..", "..", "workspace-overlay.example.toml"))
	root := filepath.Join(t.TempDir(), "workspace")
	configFile, err := createFixture(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, configFile, string(template))
	config, err := loadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}
	selections, err := config.selections("", false, true)
	if err != nil {
		t.Fatal(err)
	}
	first := selections[0]
	worktree := filepath.Join(root, ".workspaces", "one", first.project)
	const ignoredPath = ".agents/skills/workspace-check/local.md"
	writeFixture(t, filepath.Join(worktree, ignoredPath), "untracked worktree backing file\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mountSelections(ctx, selections) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(shutdownTimeout):
			t.Error("configured mounts did not stop")
		}
		out, err := projectGit(context.Background(), worktree, "status", "--porcelain", "--untracked-files=all").CombinedOutput()
		if err != nil || !strings.Contains(string(out), ignoredPath) {
			t.Errorf("untracked backing file not restored to Git status: %s (%v)", out, err)
		}
	})
	for _, selection := range selections {
		waitMount(t, selection.target, overlayType)
		want := mustReadFile(t, filepath.Join("testdata", "workspace", selection.project, "AGENTS.md"))
		for _, source := range selection.sources {
			want = append(want, mustReadFile(t, filepath.Join(source.path, "AGENTS.md"))...)
		}
		if got := mustReadFile(t, filepath.Join(selection.target, "AGENTS.md")); !bytes.Equal(got, want) {
			t.Fatalf("configured overlay %s: got %q, want %q", selection.project, got, want)
		}
	}
	waitMount(t, worktree, overlayType)
	for _, selection := range selections {
		waitMount(t, filepath.Join(root, ".workspaces", "one", selection.project), overlayType)
		mustReadFile(t, filepath.Join(selection.target, ".agents", "skills", "workspace-check", "SKILL.md"))
		want := mustReadFile(t, filepath.Join("testdata", "workspace", selection.project, "docs", "nested", "notes.md"))
		for _, source := range selection.sources {
			want = append(want, mustReadFile(t, filepath.Join(source.path, "docs", "nested", "notes.md"))...)
		}
		if got := mustReadFile(t, filepath.Join(selection.target, "docs", "nested", "notes.md")); !bytes.Equal(got, want) {
			t.Fatalf("nested merge: %q, want %q", got, want)
		}
	}
	if out, err := projectGit(context.Background(), worktree, "check-ignore", ignoredPath).CombinedOutput(); err != nil || !strings.Contains(string(out), ignoredPath) {
		t.Fatalf("shared exclusion did not hide untracked backing file: %s (%v)", out, err)
	}
	if out, err := projectGit(context.Background(), first.target, "ls-files", "AGENTS.md").CombinedOutput(); err != nil || string(out) != "AGENTS.md\n" {
		t.Fatalf("tracked collision lost: %s (%v)", out, err)
	}
	document := mustReadFile(t, filepath.Join(first.target, "AGENTS.md"))
	if got := mustReadFile(t, filepath.Join(worktree, "AGENTS.md")); !bytes.Equal(got, document) {
		t.Fatalf("worktree overlay: got %q, want %q", got, document)
	}
	finalSource := filepath.Join(first.sources[len(first.sources)-1].path, "AGENTS.md")
	originalContribution := mustReadFile(t, finalSource)
	addition := []byte("updated through the project\n")
	updated := append(bytes.Clone(document), addition...)
	if err := os.WriteFile(filepath.Join(first.target, "AGENTS.md"), updated, 0644); err != nil {
		t.Fatal(err)
	}
	if got := mustReadFile(t, finalSource); !bytes.Equal(got, append(originalContribution, addition...)) {
		t.Fatalf("most-specific contribution: %q", got)
	}
	if got := mustReadFile(t, filepath.Join(worktree, "AGENTS.md")); !bytes.Equal(got, updated) {
		t.Fatalf("worktree after save: got %q, want %q", got, updated)
	}
}
