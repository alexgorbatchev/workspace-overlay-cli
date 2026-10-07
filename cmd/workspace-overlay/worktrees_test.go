package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func initGitRepoWithCommit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
	_ = exec.Command("git", "-C", dir, "config", "user.name", "Test User").Run()
	_ = exec.Command("git", "-C", dir, "config", "user.email", "test@example.com").Run()
	dummyFile := filepath.Join(dir, "init.txt")
	if err := os.WriteFile(dummyFile, []byte("init"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("git", "-C", dir, "add", "init.txt").Run()
	cmd = exec.Command("git", "-C", dir, "commit", "-m", "initial commit")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit in %s: %v: %s", dir, err, out)
	}
}

func TestProjectTargets(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// 1. worktrees false returns single target
	nonGitDir := filepath.Join(tmpDir, "nongit")
	_ = os.Mkdir(nonGitDir, 0755)
	targets, err := projectTargets(ctx, tmpDir, "nongit", false)
	if err != nil || len(targets) != 1 {
		t.Fatalf("expected 1 target for non-git dir, got %v, err %v", targets, err)
	}

	// 2. target without .git but worktrees true returns single target
	targets, err = projectTargets(ctx, tmpDir, "nongit", true)
	if err != nil || len(targets) != 1 {
		t.Fatalf("expected 1 target for non-git dir with worktrees true, got %v, err %v", targets, err)
	}

	// 3. git repo with worktrees discovered
	gitDir := filepath.Join(tmpDir, "repo")
	initGitRepoWithCommit(t, gitDir)

	worktreeDir := filepath.Join(tmpDir, "repo-wt")
	cmd := exec.Command("git", "-C", gitDir, "worktree", "add", worktreeDir, "-b", "wt-branch")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	targets, err = projectTargets(ctx, tmpDir, "repo", true)
	if err != nil {
		t.Fatalf("projectTargets with worktrees: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d: %v", len(targets), targets)
	}

	// 4. Prunable worktree: remove worktree directory from disk without git prune
	_ = os.RemoveAll(worktreeDir)
	targets, err = projectTargets(ctx, tmpDir, "repo", true)
	if err != nil {
		t.Fatalf("projectTargets with prunable worktree: %v", err)
	}
	if len(targets) != 1 {
		t.Errorf("expected prunable worktree to be skipped, got %v", targets)
	}

	// Worktrees false on the same repo returns 1 target
	targets, err = projectTargets(ctx, tmpDir, "repo", false)
	if err != nil || len(targets) != 1 {
		t.Fatalf("expected 1 target when worktrees false, got %v, err %v", targets, err)
	}
}

func TestManageProjects(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")
	initGitRepoWithCommit(t, repoDir)

	wtDir := filepath.Join(tmpDir, "repo-wt")
	cmd := exec.Command("git", "-C", repoDir, "worktree", "add", wtDir, "-b", "wt-branch")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	// 1. Status single project
	var buf bytes.Buffer
	sel := mountSelection{root: tmpDir, project: "repo", worktrees: false}
	if err := manageProjects(ctx, &buf, "status", sel); err != nil {
		t.Fatalf("manageProjects status single: %v", err)
	}
	if strings.TrimSpace(buf.String()) != "unmounted" {
		t.Errorf("status single got %q, want %q", strings.TrimSpace(buf.String()), "unmounted")
	}

	// 2. Status with worktrees
	buf.Reset()
	sel.worktrees = true
	if err := manageProjects(ctx, &buf, "status", sel); err != nil {
		t.Fatalf("manageProjects status worktrees: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines in worktrees status, got %d: %v", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.HasSuffix(line, "\tunmounted") {
			t.Errorf("expected line to end with \tunmounted, got %q", line)
		}
	}

	// 3. Unmount on unmounted projects succeeds
	sel.worktrees = true
	if err := manageProjects(ctx, &buf, "unmount", sel); err != nil {
		t.Fatalf("manageProjects unmount: %v", err)
	}

	// 4. Invalid project returns error
	badSel := mountSelection{root: tmpDir, project: "", worktrees: false}
	if err := manageProjects(ctx, &buf, "status", badSel); err == nil {
		t.Errorf("manageProjects with bad project should return error")
	}

	// 5. Canceled context in manageProjects returns error
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manageProjects(canceledCtx, &buf, "status", sel); err == nil {
		t.Errorf("manageProjects with canceled context should return error")
	}
}

func TestMountProjectsValidation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// 1. Invalid project name returns error
	sel := mountSelection{
		root:    tmpDir,
		project: "invalid/project",
	}
	err := mountProjects(ctx, sel)
	if err == nil || !strings.Contains(err.Error(), "project must be a directory name") {
		t.Errorf("expected invalid project name error, got %v", err)
	}

	// 2. Non-existent project returns error
	sel = mountSelection{
		root:    tmpDir,
		project: "nonexistent",
	}
	err = mountProjects(ctx, sel)
	if err == nil {
		t.Errorf("expected error for nonexistent project, got nil")
	}

	// 3. Missing layer directory returns open layer error
	projDir := filepath.Join(tmpDir, "proj")
	_ = os.MkdirAll(projDir, 0755)
	sel = mountSelection{
		root:    tmpDir,
		sources: []overlaySource{{name: "missing", path: filepath.Join(tmpDir, "missing"), glob: "**/*"}},
		project: "proj",
	}
	err = mountProjects(ctx, sel)
	if err == nil || !strings.Contains(err.Error(), "open layer") {
		t.Errorf("expected open layer error for missing layer, got %v", err)
	}

	// 4. Target already mounted as non-overlay filesystem
	sel = mountSelection{
		root:    "/",
		project: "proc",
		replace: true,
	}
	err = mountProjects(ctx, sel)
	if err == nil || !strings.Contains(err.Error(), "requires --replace") {
		t.Errorf("expected non-overlay mount error for /proc, got %v", err)
	}
}

func TestMountProjectsLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "proj")
	sharedDir := filepath.Join(tmpDir, ".ai", "shared")
	specDir := filepath.Join(tmpDir, ".ai", "proj")

	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(specDir, 0755); err != nil {
		t.Fatal(err)
	}

	initGitRepoWithCommit(t, projDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sel := mountSelection{
		root:    tmpDir,
		sources: []overlaySource{{name: "shared", path: sharedDir, glob: "**/*"}, {name: "project", path: specDir, glob: "**/*"}},
		project: "proj",
		replace: false,
	}

	mountErr := make(chan error, 1)
	go func() {
		mountErr <- mountProjects(ctx, sel)
	}()

	var mounted bool
	for range 20 {
		time.Sleep(50 * time.Millisecond)
		kind, err := mountedType(context.Background(), projDir)
		if err == nil && kind == overlayType {
			mounted = true
			break
		}
	}

	if !mounted {
		cancel()
		t.Skip("FUSE mount not available or timed out in test environment")
		return
	}

	// Attempting to mount without replace while already mounted returns error
	err := mountProjects(context.Background(), sel)
	if err == nil || !strings.Contains(err.Error(), "requires --replace") {
		t.Errorf("expected requires --replace error, got %v", err)
	}

	// Test unmount via mountProjects with replace: true on a canceled context
	canceledCtx, cancelMount := context.WithCancel(context.Background())
	cancelMount()
	selReplace := sel
	selReplace.replace = true
	_ = mountProjects(canceledCtx, selReplace)

	// Test unmountOverlay on active mount
	if err := unmountOverlay(context.Background(), projDir); err != nil {
		t.Fatalf("unmountOverlay on active mount error = %v", err)
	}

	select {
	case err := <-mountErr:
		if err != nil {
			t.Errorf("mountProjects returned unexpected error after unmount: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("timeout waiting for mountProjects to exit after unmount")
	}
}

func TestWorktreeEdgeCases(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// 1. Corrupt .git file causes git worktree list to fail
	corruptDir := filepath.Join(tmpDir, "corrupt")
	_ = os.MkdirAll(corruptDir, 0755)
	_ = os.WriteFile(filepath.Join(corruptDir, ".git"), []byte("corrupt-git-file"), 0644)
	_, err := projectTargets(ctx, tmpDir, "corrupt", true)
	if err == nil || !strings.Contains(err.Error(), "discover worktrees") {
		t.Errorf("expected discover worktrees error for corrupt .git, got %v", err)
	}

	// 2. mountProjects with a missing project
	err = mountProjects(ctx, mountSelection{root: tmpDir, project: "proj"})
	if err == nil {
		t.Errorf("expected error for missing project")
	}

	// 3. mountProjects when project name is invalid
	err = mountProjects(ctx, mountSelection{root: tmpDir, project: "invalid/proj"})
	if err == nil {
		t.Errorf("expected error for invalid project name")
	}

	// 4. mountProjects when projectTargets fails
	err = mountProjects(ctx, mountSelection{root: tmpDir, project: "corrupt", worktrees: true})
	if err == nil {
		t.Errorf("expected error when projectTargets fails")
	}

	// 5. mountProjects when openExclude fails (fake .git directory)
	fakeGitDir := filepath.Join(tmpDir, "fake_git_proj")
	_ = os.MkdirAll(filepath.Join(fakeGitDir, ".git"), 0755)
	_ = os.MkdirAll(filepath.Join(tmpDir, ".ai", "ws"), 0755)
	_ = os.MkdirAll(filepath.Join(tmpDir, ".ai", "fake_git_proj"), 0755)
	err = mountProjects(ctx, mountSelection{root: tmpDir, project: "fake_git_proj"})
	if err == nil {
		t.Errorf("expected error when openExclude fails")
	}
}
