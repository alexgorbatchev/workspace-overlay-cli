package overlayfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitexclude"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestFilteredGitExclusions(t *testing.T) {
	v, base, shared, _ := setupTestView(t)
	scratch.GitRepo(t, base)
	scratch.Write(t, filepath.Join(shared, "docs/matched.md"), "selected")
	scratch.Write(t, filepath.Join(shared, "hidden/file.txt"), "excluded")
	scratch.Write(t, filepath.Join(shared, "hidden.txt"), "excluded")
	v.layers[1].rules = &pathRules{glob: "**/*.md"}
	g, err := gitexclude.Open(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	v.exclude = g
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		ignored bool
	}{{"docs/matched.md", true}, {"hidden/file.txt", false}, {"hidden.txt", false}} {
		cmd := exec.Command("git", "-C", base, "check-ignore", "--no-index", tt.name)
		if err := cmd.Run(); (err == nil) != tt.ignored {
			t.Fatalf("Git ignore %s: %v, want ignored=%v", tt.name, err, tt.ignored)
		}
	}
	if err := v.layers[1].root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := v.RefreshPaths(); err == nil {
		t.Fatal("unreadable source excluded silently")
	}
}

func TestGlobLayers(t *testing.T) {
	v, base, shared, specific := setupTestView(t)
	v.layers[1].rules = &pathRules{glob: "**/*.md"}
	v.layers[2].rules = &pathRules{glob: "**/*"}
	scratch.Write(t, filepath.Join(base, "doc.md"), "base-")
	scratch.Write(t, filepath.Join(shared, "doc.md"), "shared-")
	scratch.Write(t, filepath.Join(specific, "doc.md"), "project")
	scratch.Write(t, filepath.Join(shared, "nested/deep/notes.md"), "nested")
	scratch.Write(t, filepath.Join(shared, "nested/deep/hidden.txt"), "hidden")
	scratch.Write(t, filepath.Join(shared, ".agents/skills/demo/SKILL.md"), "skill")
	scratch.Write(t, filepath.Join(shared, ".git/config"), "bad")
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	parts, err := v.resolve("doc.md")
	if err != nil {
		t.Fatal(err)
	}
	content, err := v.contents("doc.md", parts)
	if err != nil || string(content) != "base-shared-project" {
		t.Fatalf("concatenated selection: %s, %v", content, err)
	}
	for _, name := range []string{"nested/deep/notes.md", ".agents/skills/demo/SKILL.md"} {
		if _, err := v.resolve(name); err != nil {
			t.Fatalf("selected %s: %v", name, err)
		}
	}
	for _, name := range []string{"nested/deep/hidden.txt", ".git/config"} {
		if _, err := v.resolve(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("excluded %s: %v", name, err)
		}
	}
	entries, err := v.entries("nested/deep")
	if err != nil || len(entries) != 1 || entries[0].Name() != "notes.md" {
		t.Fatalf("glob filtered entries: %v, %v", entries, err)
	}
	if _, err := v.destination("nested/deep/new.txt"); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("write outside glob: %v", err)
	}
	if index, err := v.destination("nested/deep/new.md"); err != nil || index != 1 {
		t.Fatalf("write within glob: %d, %v", index, err)
	}
	scratch.Write(t, filepath.Join(shared, "nested/deep/new.md"), "new")
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	if _, err := v.resolve("nested/deep/new.md"); err != nil {
		t.Fatal(err)
	}
	v.layers[1].rules.glob = "["
	if err := v.RefreshPaths(); err == nil {
		t.Fatal("invalid glob accepted")
	}
}
