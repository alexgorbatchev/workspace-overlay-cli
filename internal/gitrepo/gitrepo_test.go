package gitrepo

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMain(m *testing.M) {
	scratch.Main(m)
}

func TestCommandNeverUsesEnclosingRepository(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	scratch.Mkdir(t, outer)
	scratch.GitRepo(t, outer)
	inner := filepath.Join(outer, "inner")
	scratch.Mkdir(t, inner)
	scratch.Write(t, filepath.Join(inner, ".git"), "not a repository")
	if err := Command(ctx, inner, "rev-parse", "--git-dir").Run(); err == nil {
		t.Fatal("Command with fake .git should fail")
	}
	output, err := Command(ctx, outer, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil {
		t.Fatalf("Command on valid repo failed: %v", err)
	}
	if string(output) != "true\n" {
		t.Errorf("expected 'true\\n', got %q", string(output))
	}
}
