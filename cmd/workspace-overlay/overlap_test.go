package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGroupedSourceOverlap(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	selections := []mountSelection{{root: root, project: "first", target: first, sources: []overlaySource{{path: second, glob: "**/*"}}}, {root: root, project: "second", target: second}}
	if err := mountSelections(context.Background(), selections); err == nil || !strings.Contains(err.Error(), "inside") {
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
	selections := []mountSelection{{root: root, project: "project", target: target}, {root: root, project: "alias", target: alias}}
	if err := mountSelections(context.Background(), selections); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("aliased duplicate target: %v", err)
	}
}
