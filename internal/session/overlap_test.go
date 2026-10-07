package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
)

func TestGroupedSourceOverlap(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	selections := []Selection{{Root: root, Project: "first", Target: first, Sources: []config.Source{{Path: second, Glob: "**/*"}}}, {Root: root, Project: "second", Target: second}}
	if err := Mount(context.Background(), selections); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Fatalf("source underneath another project's mount: %v", err)
	}
}

func TestAliasedMountOverlap(t *testing.T) {
	root := t.TempDir()
	target, alias := filepath.Join(root, "project"), filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	selections := []Selection{{Root: root, Project: "project", Target: target}, {Root: root, Project: "alias", Target: alias}}
	if err := Mount(context.Background(), selections); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("aliased duplicate target: %v", err)
	}
}
