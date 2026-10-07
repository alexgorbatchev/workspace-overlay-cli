package session

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/fixture"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitrepo"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestExampleConfigurationMountsAndWrites(t *testing.T) {
	const shutdownTimeout = 5 * time.Second
	template := scratch.Read(t, filepath.Join("..", "..", "workspace-overlay.example.toml"))
	root := filepath.Join(t.TempDir(), "workspace")
	configFile, err := fixture.Create(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	scratch.Write(t, configFile, string(template))
	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := Selections(cfg, "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	first := mounts[0]
	worktree := filepath.Join(root, ".workspaces", "one", first.Project)
	const ignoredPath = ".agents/skills/workspace-check/local.md"
	scratch.Write(t, filepath.Join(worktree, ignoredPath), "untracked worktree backing file\n")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Mount(ctx, mounts) }()
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
		out, err := gitrepo.Command(context.Background(), worktree, "status", "--porcelain", "--untracked-files=all").CombinedOutput()
		if err != nil || !strings.Contains(string(out), ignoredPath) {
			t.Errorf("untracked backing file not restored to Git status: %s (%v)", out, err)
		}
	})
	for _, selection := range mounts {
		waitMount(t, selection.Target, overlayfs.FilesystemType)
		want := scratch.Read(t, filepath.Join("..", "fixture", "testdata", "workspace", selection.Project, "AGENTS.md"))
		for _, source := range selection.Sources {
			want = append(want, scratch.Read(t, filepath.Join(source.Path, "AGENTS.md"))...)
		}
		if got := scratch.Read(t, filepath.Join(selection.Target, "AGENTS.md")); !bytes.Equal(got, want) {
			t.Fatalf("configured overlay %s: got %q, want %q", selection.Project, got, want)
		}
	}
	waitMount(t, worktree, overlayfs.FilesystemType)
	for _, selection := range mounts {
		waitMount(t, filepath.Join(root, ".workspaces", "one", selection.Project), overlayfs.FilesystemType)
		scratch.Read(t, filepath.Join(selection.Target, ".agents", "skills", "workspace-check", "SKILL.md"))
		want := scratch.Read(t, filepath.Join("..", "fixture", "testdata", "workspace", selection.Project, "docs", "nested", "notes.md"))
		for _, source := range selection.Sources {
			want = append(want, scratch.Read(t, filepath.Join(source.Path, "docs", "nested", "notes.md"))...)
		}
		if got := scratch.Read(t, filepath.Join(selection.Target, "docs", "nested", "notes.md")); !bytes.Equal(got, want) {
			t.Fatalf("nested merge: %q, want %q", got, want)
		}
	}
	if out, err := gitrepo.Command(context.Background(), worktree, "check-ignore", ignoredPath).CombinedOutput(); err != nil || !strings.Contains(string(out), ignoredPath) {
		t.Fatalf("shared exclusion did not hide untracked backing file: %s (%v)", out, err)
	}
	if out, err := gitrepo.Command(context.Background(), first.Target, "ls-files", "AGENTS.md").CombinedOutput(); err != nil || string(out) != "AGENTS.md\n" {
		t.Fatalf("tracked collision lost: %s (%v)", out, err)
	}
	document := scratch.Read(t, filepath.Join(first.Target, "AGENTS.md"))
	if got := scratch.Read(t, filepath.Join(worktree, "AGENTS.md")); !bytes.Equal(got, document) {
		t.Fatalf("worktree overlay: got %q, want %q", got, document)
	}
	finalSource := filepath.Join(first.Sources[len(first.Sources)-1].Path, "AGENTS.md")
	originalContribution := scratch.Read(t, finalSource)
	addition := []byte("updated through the project\n")
	updated := append(bytes.Clone(document), addition...)
	if err := os.WriteFile(filepath.Join(first.Target, "AGENTS.md"), updated, 0644); err != nil {
		t.Fatal(err)
	}
	if got := scratch.Read(t, finalSource); !bytes.Equal(got, append(originalContribution, addition...)) {
		t.Fatalf("most-specific contribution: %q", got)
	}
	if got := scratch.Read(t, filepath.Join(worktree, "AGENTS.md")); !bytes.Equal(got, updated) {
		t.Fatalf("worktree after save: got %q, want %q", got, updated)
	}
}
