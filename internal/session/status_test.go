package session

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

func TestStatusAlwaysNamesTarget(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := manageProjects(context.Background(), &out, "status", Selection{Root: root, Project: "project", Target: project, Worktrees: false}); err != nil {
		t.Fatalf("manageProjects error: %v", err)
	}
	expected := pathname.Display(project) + "\tunmounted\n"
	if out.String() != expected {
		t.Errorf("got %q, want %q", out.String(), expected)
	}
}
