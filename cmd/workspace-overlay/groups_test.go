package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGroupedMounts(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, ".ai/common")
	writeFixture(t, filepath.Join(source, "AGENTS.md"), "common")
	var selections []mountSelection
	for _, name := range []string{"first", "second"} {
		target := filepath.Join(root, name)
		if err := os.Mkdir(target, 0755); err != nil {
			t.Fatal(err)
		}
		selections = append(selections, mountSelection{root: root, project: name, target: target, sources: []overlaySource{{name: "common", path: source, glob: "**/*"}}})
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
		case <-time.After(5 * time.Second):
			t.Error("group did not stop")
		}
	})
	for _, s := range selections {
		waitMount(t, s.target, overlayType)
		if got := string(mustReadFile(t, filepath.Join(s.target, "AGENTS.md"))); got != "common" {
			t.Fatalf("shared group overlay: %s", got)
		}
	}
	if err := ensureWorktreeIsolation(context.Background(), selections[1].target, nil); err == nil {
		t.Fatal("foreign project mount overlap accepted")
	}
	if err := ensureWorktreeIsolation(context.Background(), source, []overlaySource{{path: source}}); err == nil {
		t.Fatal("new worktree contains its own overlay source")
	}
	ctxCanceled, cancelProbe := context.WithCancel(context.Background())
	cancelProbe()
	if err := ensureWorktreeIsolation(ctxCanceled, root, nil); err == nil {
		t.Fatal("canceled mount inspection accepted")
	}
	// Source additions are discovered from native filesystem notifications.
	writeFixture(t, filepath.Join(source, "deep/new/file.md"), "live")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(selections[1].target, "deep/new/file.md"))
		if err == nil && string(data) == "live" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got := string(mustReadFile(t, filepath.Join(selections[1].target, "deep/new/file.md"))); got != "live" {
		t.Fatalf("live nested overlay: %s", got)
	}
	if err := os.Remove(filepath.Join(source, "deep/new/file.md")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(selections[1].target, "deep/new/file.md")); os.IsNotExist(err) {
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
		selections []mountSelection
		want       string
	}{
		{"same target", []mountSelection{{target: target}, {target: target}}, "overlap"},
		{"nested target", []mountSelection{{target: target}, {target: filepath.Join(target, "child")}}, "overlap"},
		{"source in target", []mountSelection{{target: target, sources: []overlaySource{{path: filepath.Join(target, "source")}}}}, "inside"},
		{"missing source", []mountSelection{{root: root, project: "project", target: target, sources: []overlaySource{{path: filepath.Join(root, "missing"), glob: "**/*"}}}}, "open layer"},
		{"missing worktree metadata", []mountSelection{{root: root, project: "missing", target: filepath.Join(root, "missing"), worktrees: true}}, "no such file"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if err := mountSelections(context.Background(), tt.selections); err == nil || !strings.Contains(err.Error(), tt.want) {
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
	selections := []mountSelection{{root: root, project: "first", target: first}, {root: root, project: "second", target: "/proc"}}
	if err := mountSelections(context.Background(), selections); err == nil {
		t.Fatal("failed group accepted")
	}
	waitMount(t, first, "")
}
