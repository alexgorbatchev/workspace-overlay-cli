package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestGroupedMounts(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, ".ai/common")
	scratch.Write(t, filepath.Join(source, "AGENTS.md"), "common")
	var selections []Selection
	for _, name := range []string{"first", "second"} {
		target := filepath.Join(root, name)
		if err := os.Mkdir(target, 0755); err != nil {
			t.Fatal(err)
		}
		selections = append(selections, Selection{Root: root, Project: name, Target: target, Sources: []config.Source{{Name: "common", Path: source, Glob: "**/*"}}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Mount(ctx, selections) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("group did not stop")
		}
	})
	for _, s := range selections {
		waitMount(t, s.Target, overlayfs.FilesystemType)
		if got := string(scratch.Read(t, filepath.Join(s.Target, "AGENTS.md"))); got != "common" {
			t.Fatalf("shared group overlay: %s", got)
		}
	}
	if err := ensureWorktreeIsolation(context.Background(), selections[1].Target, nil); err == nil {
		t.Fatal("foreign project mount overlap accepted")
	}
	if err := ensureWorktreeIsolation(context.Background(), source, []config.Source{{Path: source}}); err == nil {
		t.Fatal("new worktree contains its own overlay source")
	}
	ctxCanceled, cancelProbe := context.WithCancel(context.Background())
	cancelProbe()
	if err := ensureWorktreeIsolation(ctxCanceled, root, nil); err == nil {
		t.Fatal("canceled mount inspection accepted")
	}
	// Source additions are discovered from native filesystem notifications.
	scratch.Write(t, filepath.Join(source, "deep/new/file.md"), "live")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(selections[1].Target, "deep/new/file.md"))
		if err == nil && string(data) == "live" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := string(scratch.Read(t, filepath.Join(selections[1].Target, "deep/new/file.md"))); got != "live" {
		t.Fatalf("live nested overlay: %s", got)
	}
	if err := os.Remove(filepath.Join(source, "deep/new/file.md")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(selections[1].Target, "deep/new/file.md")); os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("source deletion not reflected")
}

func TestGroupedMountValidation(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "project")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"child", "source"} {
		if err := os.Mkdir(filepath.Join(target, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name       string
		selections []Selection
		want       string
	}{
		{"same target", []Selection{{Target: target}, {Target: target}}, "overlap"},
		{"nested target", []Selection{{Target: target}, {Target: filepath.Join(target, "child")}}, "overlap"},
		{"source in target", []Selection{{Target: target, Sources: []config.Source{{Path: filepath.Join(target, "source")}}}}, "inside"},
		{"missing source", []Selection{{Root: root, Project: "project", Target: target, Sources: []config.Source{{Path: filepath.Join(root, "missing"), Glob: "**/*"}}}}, "open layer"},
		{"missing worktree metadata", []Selection{{Root: root, Project: "missing", Target: filepath.Join(root, "missing"), Worktrees: true}}, "no such file"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if err := Mount(context.Background(), tt.selections); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("mountSelections: %v, want %s", err, tt.want)
			}
		})
	}
}

func TestGroupedMountCancelsOnFailure(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	if err := os.Mkdir(first, 0755); err != nil {
		t.Fatal(err)
	}
	selections := []Selection{{Root: root, Project: "first", Target: first}, {Root: root, Project: "second", Target: "/proc"}}
	if err := Mount(context.Background(), selections); err == nil {
		t.Fatal("failed group accepted")
	}
	waitMount(t, first, "")
}
