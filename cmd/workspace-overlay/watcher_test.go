package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitMount(t *testing.T, target, expected string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		kind, err := mountedType(context.Background(), target)
		if err == nil && kind == expected {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mount %s did not reach %q", target, expected)
}

func TestLiveWorktreeDiscovery(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	initGitRepoWithCommit(t, project)
	shared := filepath.Join(root, ".ai", "shared")
	specific := filepath.Join(root, ".ai", "project")
	for _, dir := range []string{shared, specific} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, "shared.txt"), []byte("shared"), 0644); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(project, ".git", "info", "exclude")
	original, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	selection := mountSelection{root: root, project: "project", worktrees: true,
		sources: []overlaySource{{name: "shared", path: shared, glob: "**/*"}, {name: "project", path: specific, glob: "**/*"}}}
	go func() { finished <- mountProjects(ctx, selection) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watcher did not stop")
		}
	})
	waitMount(t, project, overlayType)
	worktree := filepath.Join(root, ".workspaces", "live")
	cmd := exec.Command("git", "-C", project, "worktree", "add", "-b", "live", worktree, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		ref, readErr := os.ReadFile(filepath.Join(project, ".git", "refs", "heads", "live"))
		t.Logf("new branch reference: %q (%v)", ref, readErr)
		refs, refsErr := exec.Command("git", "-C", project, "show-ref").CombinedOutput()
		t.Logf("Git references: %s (%v)", refs, refsErr)
		t.Fatalf("add worktree: %v: %s", err, out)
	}
	waitMount(t, worktree, overlayType)
	data, err := os.ReadFile(filepath.Join(worktree, "shared.txt"))
	if err != nil || string(data) != "shared" {
		t.Fatalf("shared overlay: %q, %v", data, err)
	}
	if err := unmountOverlay(context.Background(), worktree); err != nil {
		t.Fatal(err)
	}
	waitMount(t, worktree, "")
	// An explicit unmount stays suppressed while the registration still exists.
	time.Sleep(300 * time.Millisecond)
	waitMount(t, worktree, "")
	cmd = exec.Command("git", "-C", worktree, "rev-parse", "--absolute-git-dir")
	metadata, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	// Unregister without deleting through a writable mounted tree.
	name := string(metadata[:len(metadata)-1])
	parked := filepath.Join(project, ".git", "parked-metadata")
	if err := os.Rename(name, parked); err != nil {
		t.Fatal(err)
	}
	waitMount(t, worktree, "")
	time.Sleep(300 * time.Millisecond)
	if err := os.Rename(parked, name); err != nil {
		t.Fatal(err)
	}
	waitMount(t, worktree, overlayType)
	if err := os.Rename(name, parked); err != nil {
		t.Fatal(err)
	}
	waitMount(t, worktree, "")
	if err := os.Rename(parked, name); err != nil {
		t.Fatal(err)
	}
	if err := manageProjects(context.Background(), os.Stdout, "unmount", selection); err != nil {
		t.Fatal(err)
	}
	waitMount(t, project, "")
	// Allow the owner to finish removing its managed exclusion blocks.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(exclude)
		if err == nil && string(data) == string(original) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("exclude file was not restored")
}

func TestNestedWorktreeRejected(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	initGitRepoWithCommit(t, project)
	selection := mountSelection{root: root, project: "project", target: project, worktrees: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- mountProjects(ctx, selection) }()
	waitMount(t, project, overlayType)
	cmd := exec.Command("git", "-C", project, "worktree", "add", "-b", "nested", filepath.Join(project, "nested"), "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create nested worktree: %v %s", err, out)
	}
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "overlaps") {
			t.Fatalf("nested live mount: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("nested mount was not rejected")
	}
	waitMount(t, project, "")
}
