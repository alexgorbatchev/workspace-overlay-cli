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

// markedDocument returns what a mounted project shows for name: the project's
// own text, then each overlay's text between the built-in Markdown markers.
func markedDocument(t *testing.T, selection Selection, project []byte, name string) []byte {
	t.Helper()
	want := bytes.Clone(project)
	for _, source := range selection.Sources {
		path, err := filepath.Rel(selection.Target, filepath.Join(source.Path, name))
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, "<!-- BEGIN WORKSPACE-OVERLAY: "+source.Name+" ("+path+") -->\n"...)
		want = append(want, scratch.Read(t, filepath.Join(source.Path, name))...)
		want = append(want, "<!-- END WORKSPACE-OVERLAY: "+source.Name+" ("+path+") -->\n"...)
	}
	return want
}

// The shipped configuration names no marker rules, so the built-in ones apply.
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
		want := markedDocument(t, selection, scratch.Read(t, filepath.Join("..", "fixture", "testdata", "workspace", selection.Project, "AGENTS.md")), "AGENTS.md")
		if got := scratch.Read(t, filepath.Join(selection.Target, "AGENTS.md")); !bytes.Equal(got, want) {
			t.Fatalf("configured overlay %s: got %q, want %q", selection.Project, got, want)
		}
	}
	waitMount(t, worktree, overlayfs.FilesystemType)
	for _, selection := range mounts {
		waitMount(t, filepath.Join(root, ".workspaces", "one", selection.Project), overlayfs.FilesystemType)
		scratch.Read(t, filepath.Join(selection.Target, ".agents", "skills", "workspace-check", "SKILL.md"))
		nested := filepath.Join("docs", "nested", "notes.md")
		want := markedDocument(t, selection, scratch.Read(t, filepath.Join("..", "fixture", "testdata", "workspace", selection.Project, nested)), nested)
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
	// A worktree shows the same document; its markers name the sources by
	// their path from the worktree.
	checkout := first
	checkout.Target = worktree
	projectText := scratch.Read(t, filepath.Join("..", "fixture", "testdata", "workspace", first.Project, "AGENTS.md"))
	if got, want := scratch.Read(t, filepath.Join(worktree, "AGENTS.md")), markedDocument(t, checkout, projectText, "AGENTS.md"); !bytes.Equal(got, want) {
		t.Fatalf("worktree overlay: got %q, want %q", got, want)
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
	// The appended line now lies inside the section of the source that took it.
	saved := markedDocument(t, checkout, projectText, "AGENTS.md")
	if got := scratch.Read(t, filepath.Join(worktree, "AGENTS.md")); !bytes.Equal(got, saved) {
		t.Fatalf("worktree after save: got %q, want %q", got, saved)
	}
}
