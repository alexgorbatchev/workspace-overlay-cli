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

	// Create outer repository
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	scratch.Mkdir(t, outer)
	scratch.GitRepo(t, outer)

	// Create an inner directory with a FILE named .git (not a repo)
	inner := filepath.Join(outer, "inner")
	scratch.Mkdir(t, inner)
	gitFile := filepath.Join(inner, ".git")
	scratch.Write(t, gitFile, "not a repository")

	// Command to the inner directory (with fake .git file) should fail
	cmd := Command(ctx, inner, "rev-parse", "--git-dir")
	if err := cmd.Run(); err == nil {
		t.Fatal("Command with fake .git should fail")
	}

	// Command to outer repository should succeed
	cmd = Command(ctx, outer, "rev-parse", "--is-inside-work-tree")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("Command on valid repo failed: %v", err)
	}
	if string(output) != "true\n" {
		t.Errorf("expected 'true\\n', got %q", string(output))
	}
}
