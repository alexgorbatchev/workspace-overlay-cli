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

	kind, err := mountedType(context.Background(), filepath.Join(project, "nested"))
	if err != nil {
		t.Fatalf("check nested mount: %v", err)
	}
	if kind != "" {
		t.Fatalf("nested worktree should not be mounted, got %q", kind)
	}

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
	accepted := []string{"/tmp/project", "/tmp/other"}
	tests := []struct {
		worktree    string
		source      string
		unsupported bool
	}{
		{"/tmp/project/nested", "/tmp/source", true},      // inside an accepted target
		{"/tmp", "/tmp/source", true},                     // contains an accepted target
		{"/tmp/container", "/tmp/container/source", true}, // contains an overlay source
		{"/tmp/valid/worktree", "/tmp/source", false},
	}
	for _, tt := range tests {
		reason := unsupportedWorktree(tt.worktree, accepted, []string{tt.source})
		if (reason != "") != tt.unsupported {
			t.Errorf("unsupportedWorktree(%q) = %q, want unsupported=%v", tt.worktree, reason, tt.unsupported)
		}
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
