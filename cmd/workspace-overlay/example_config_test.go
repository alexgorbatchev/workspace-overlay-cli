package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExampleConfigurationMountsAndWrites(t *testing.T) {
	const shutdownTimeout = 5 * time.Second
	template := mustReadFile(t, filepath.Join("..", "..", "workspace-overlay.example.toml"))
	root := t.TempDir()
	configFile := filepath.Join(root, configName)
	writeFixture(t, configFile, string(template))
	config, err := loadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range config.Projects {
		initGitRepoWithCommit(t, project.Path)
	}
	for _, source := range config.Overlays {
		writeFixture(t, filepath.Join(source.Source, "AGENTS.md"), source.Name+"\n")
	}
	selections, err := config.selections("", false, true)
	if err != nil {
		t.Fatal(err)
	}
	first := selections[0]
	worktree := filepath.Join(root, ".workspaces", first.project)
	cmd := projectGit(context.Background(), first.target, "worktree", "add", "--detach", worktree, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add worktree: %v: %s", err, out)
	}
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
	})
	for _, selection := range selections {
		waitMount(t, selection.target, overlayType)
		var want []byte
		for _, source := range selection.sources {
			want = append(want, mustReadFile(t, filepath.Join(source.path, "AGENTS.md"))...)
		}
		if got := mustReadFile(t, filepath.Join(selection.target, "AGENTS.md")); !bytes.Equal(got, want) {
			t.Fatalf("configured overlay %s: got %q, want %q", selection.project, got, want)
		}
	}
	waitMount(t, worktree, overlayType)
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
