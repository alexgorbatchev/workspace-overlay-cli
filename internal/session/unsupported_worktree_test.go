package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestStartupSkipsNestedWorktree(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)

	// Create a nested worktree BEFORE mounting
	cmd := exec.Command("git", "-C", project, "worktree", "add", "-b", "nested", filepath.Join(project, "nested"), "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create nested worktree: %v: %s", err, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
	})

	finished := make(chan error, 1)
	selection := Selection{Root: root, Project: "project", Target: project, Worktrees: true}
	go func() { finished <- Mount(ctx, []Selection{selection}) }()

	// Wait up to 5 seconds for the project to be mounted
	deadline := time.Now().Add(5 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var mounted bool
	for time.Now().Before(deadline) {
		<-ticker.C
		kind, err := mountedType(context.Background(), project)
		if err == nil && kind == overlayfs.FilesystemType {
			mounted = true
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("goroutine returned early with error: %v", err)
		default:
		}
	}
	if !mounted {
		cancel()
		select {
		case err := <-finished:
			t.Fatalf("mount failed: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("mount did not succeed")
		}
	}

	// Assert the nested worktree is not a mount of its own
	kind, err := mountedType(context.Background(), filepath.Join(project, "nested"))
	if err != nil {
		t.Fatalf("check nested mount: %v", err)
	}
	if kind != "" {
		t.Fatalf("nested worktree should not be mounted, got %q", kind)
	}

	// Cancel and expect a nil result within 5 seconds
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Errorf("mountSelections returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mountSelections did not stop")
	}

	waitMount(t, project, "")
}

func TestUnsupportedWorktreeFunction(t *testing.T) {
	// Test the unsupportedWorktree function directly
	accepted := []string{"/tmp/project", "/tmp/other"}
	sources := []string{"/tmp/source"}

	// Case 1: worktree overlaps with accepted target (child of accepted)
	result := unsupportedWorktree("/tmp/project/nested", accepted, sources)
	if result == "" {
		t.Error("expected overlap detection for nested worktree")
	}

	// Case 2: worktree overlaps with accepted target (parent of accepted)
	result = unsupportedWorktree("/tmp", accepted, sources)
	if result == "" {
		t.Error("expected overlap detection for parent worktree")
	}

	// Case 3: worktree contains an overlay source
	result = unsupportedWorktree("/tmp/container", accepted, []string{"/tmp/container/source"})
	if result == "" {
		t.Error("expected source containment detection")
	}

	// Case 4: valid worktree
	result = unsupportedWorktree("/tmp/valid/worktree", accepted, sources)
	if result != "" {
		t.Errorf("expected valid worktree, got reason: %s", result)
	}
}

func TestUnsupportedWorktreeOverlapsDuringReconcile(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	shared := filepath.Join(root, ".ai", "shared")
	if err := os.MkdirAll(shared, 0755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	selection := Selection{Root: root, Project: "project", Target: project, Worktrees: true,
		Sources: []config.Source{{Name: "shared", Path: shared, Glob: "**/*"}}}
	go func() { finished <- mountProjects(ctx, selection) }()

	waitMount(t, project, overlayfs.FilesystemType)

	// Create a worktree that overlaps with the project mount
	nestedPath := filepath.Join(project, "nested-overlap")
	cmd := exec.Command("git", "-C", project, "worktree", "add", "-b", "nested-overlap", nestedPath, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create nested worktree: %v: %s", err, out)
	}

	// Wait to ensure goroutine doesn't exit
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-finished:
		t.Fatalf("mount goroutine returned early: %v", err)
	default:
	}

	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Errorf("mountProjects returned error on cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mountProjects did not stop")
	}
	waitMount(t, project, "")
}
