package session

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"workspace-overlay/internal/config"
	"workspace-overlay/internal/overlayfs"
	"workspace-overlay/internal/pathname"
	"workspace-overlay/internal/scratch"
)

func TestDiscoverWorktrees(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	t.Run("non-git dir with worktrees false", func(t *testing.T) {
		nonGitDir := filepath.Join(tmpDir, "nongit")
		if err := os.Mkdir(nonGitDir, 0755); err != nil {
			t.Fatal(err)
		}
		targets, err := discoverWorktrees(ctx, filepath.Join(tmpDir, "nongit"), false)
		if err != nil || len(targets) != 1 {
			t.Fatalf("expected 1 target for non-git dir, got %v, err %v", targets, err)
		}
	})

	t.Run("non-git dir with worktrees true", func(t *testing.T) {
		targets, err := discoverWorktrees(ctx, filepath.Join(tmpDir, "nongit"), true)
		if err != nil || len(targets) != 1 {
			t.Fatalf("expected 1 target for non-git dir with worktrees true, got %v, err %v", targets, err)
		}
	})

	t.Run("git repo with worktrees", func(t *testing.T) {
		gitDir := filepath.Join(tmpDir, "repo")
		scratch.GitRepo(t, gitDir)

		worktreeDir := filepath.Join(tmpDir, "repo-wt")
		cmd := exec.Command("git", "-C", gitDir, "worktree", "add", worktreeDir, "-b", "wt-branch")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git worktree add: %v: %s", err, out)
		}

		targets, err := discoverWorktrees(ctx, filepath.Join(tmpDir, "repo"), true)
		if err != nil {
			t.Fatalf("discoverWorktrees with worktrees: %v", err)
		}
		if len(targets) != 2 {
			t.Fatalf("expected 2 targets, got %d: %v", len(targets), targets)
		}

		// Prunable worktree: remove worktree directory from disk without git prune
		if err := os.RemoveAll(worktreeDir); err != nil {
			t.Fatal(err)
		}
		targets, err = discoverWorktrees(ctx, filepath.Join(tmpDir, "repo"), true)
		if err != nil {
			t.Fatalf("discoverWorktrees with prunable worktree: %v", err)
		}
		if len(targets) != 1 {
			t.Errorf("expected prunable worktree to be skipped, got %v", targets)
		}

		// Worktrees false on the same repo returns 1 target
		targets, err = discoverWorktrees(ctx, filepath.Join(tmpDir, "repo"), false)
		if err != nil || len(targets) != 1 {
			t.Fatalf("expected 1 target when worktrees false, got %v, err %v", targets, err)
		}
	})
}

func TestManageProjects(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")
	scratch.GitRepo(t, repoDir)

	wtDir := filepath.Join(tmpDir, "repo-wt")
	cmd := exec.Command("git", "-C", repoDir, "worktree", "add", wtDir, "-b", "wt-branch")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	t.Run("status single project", func(t *testing.T) {
		var buf bytes.Buffer
		sel := Selection{Root: tmpDir, Project: "repo", Target: repoDir, Worktrees: false}
		if err := manageProjects(ctx, &buf, "status", sel); err != nil {
			t.Fatalf("manageProjects status single: %v", err)
		}
		expected := pathname.Display(repoDir) + "\tunmounted\n"
		if buf.String() != expected {
			t.Errorf("status single got %q, want %q", buf.String(), expected)
		}
	})

	t.Run("status with worktrees", func(t *testing.T) {
		var buf bytes.Buffer
		sel := Selection{Root: tmpDir, Project: "repo", Target: repoDir, Worktrees: true}
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
	})

	t.Run("unmount unmounted projects", func(t *testing.T) {
		var buf bytes.Buffer
		sel := Selection{Root: tmpDir, Project: "repo", Target: repoDir, Worktrees: true}
		if err := manageProjects(ctx, &buf, "unmount", sel); err != nil {
			t.Fatalf("manageProjects unmount: %v", err)
		}
	})

	t.Run("canceled context returns error", func(t *testing.T) {
		var buf bytes.Buffer
		sel := Selection{Root: tmpDir, Project: "repo", Target: repoDir, Worktrees: true}
		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := manageProjects(canceledCtx, &buf, "status", sel); err == nil {
			t.Errorf("manageProjects with canceled context should return error")
		}
	})
}

func TestMountProjectsValidation(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	t.Run("non-existent project", func(t *testing.T) {
		sel := Selection{
			Root:    tmpDir,
			Project: "nonexistent",
			Target:  filepath.Join(tmpDir, "nonexistent"),
		}
		err := mountProjects(ctx, sel)
		if err == nil {
			t.Errorf("expected error for nonexistent project, got nil")
		}
	})

	t.Run("missing layer directory", func(t *testing.T) {
		projDir := filepath.Join(tmpDir, "proj")
		if err := os.MkdirAll(projDir, 0755); err != nil {
			t.Fatal(err)
		}
		sel := Selection{
			Root:    tmpDir,
			Sources: []config.Source{{Name: "missing", Path: filepath.Join(tmpDir, "missing"), Glob: "**/*"}},
			Project: "proj",
			Target:  projDir,
		}
		err := mountProjects(ctx, sel)
		if err == nil || !strings.Contains(err.Error(), "open layer") {
			t.Errorf("expected open layer error for missing layer, got %v", err)
		}
	})

	t.Run("non-overlay filesystem mount", func(t *testing.T) {
		sel := Selection{
			Root:    "/",
			Project: "proc",
			Target:  "/proc",
			Replace: true,
		}
		err := mountProjects(ctx, sel)
		if err == nil || !strings.Contains(err.Error(), "refusing to replace") {
			t.Errorf("expected non-overlay mount error for /proc, got %v", err)
		}
	})
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

	scratch.GitRepo(t, projDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sel := Selection{
		Root:    tmpDir,
		Sources: []config.Source{{Name: "shared", Path: sharedDir, Glob: "**/*"}, {Name: "project", Path: specDir, Glob: "**/*"}},
		Project: "proj",
		Target:  projDir,
		Replace: false,
	}

	mountErr := make(chan error, 1)
	go func() {
		mountErr <- mountProjects(ctx, sel)
	}()

	waitMount(t, projDir, overlayfs.FilesystemType)

	// Attempting to mount without replace while already mounted returns error
	err := mountProjects(context.Background(), sel)
	if err == nil || !strings.Contains(err.Error(), "use --replace") {
		t.Errorf("expected use --replace error, got %v", err)
	}

	// Test unmount via mountProjects with replace: true on a canceled context
	canceledCtx, cancelMount := context.WithCancel(context.Background())
	cancelMount()
	selReplace := sel
	selReplace.Replace = true
	_ = mountProjects(canceledCtx, selReplace) // context canceled, error expected

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

	corruptDir := filepath.Join(tmpDir, "corrupt")
	if err := os.MkdirAll(corruptDir, 0755); err != nil {
		t.Fatal(err)
	}
	scratch.Write(t, filepath.Join(corruptDir, ".git"), "corrupt-git-file")

	t.Run("corrupt .git causes discover worktrees failure", func(t *testing.T) {
		_, err := discoverWorktrees(ctx, filepath.Join(tmpDir, "corrupt"), true)
		if err == nil || !strings.Contains(err.Error(), "discover worktrees") {
			t.Errorf("expected discover worktrees error for corrupt .git, got %v", err)
		}
	})

	t.Run("missing project", func(t *testing.T) {
		err := mountProjects(ctx, Selection{Root: tmpDir, Project: "proj", Target: filepath.Join(tmpDir, "proj")})
		if err == nil {
			t.Errorf("expected error for missing project")
		}
	})

	t.Run("mount fails when discoverWorktrees fails", func(t *testing.T) {
		err := mountProjects(ctx, Selection{Root: tmpDir, Project: "corrupt", Target: filepath.Join(tmpDir, "corrupt"), Worktrees: true})
		if err == nil {
			t.Errorf("expected error when discoverWorktrees fails")
		}
	})

	t.Run("mount fails when openExclude fails", func(t *testing.T) {
		fakeGitDir := filepath.Join(tmpDir, "fake_git_proj")
		if err := os.MkdirAll(filepath.Join(fakeGitDir, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(tmpDir, ".ai", "ws"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(tmpDir, ".ai", "fake_git_proj"), 0755); err != nil {
			t.Fatal(err)
		}
		err := mountProjects(ctx, Selection{Root: tmpDir, Project: "fake_git_proj", Target: fakeGitDir})
		if err == nil {
			t.Errorf("expected error when openExclude fails")
		}
	})
}
