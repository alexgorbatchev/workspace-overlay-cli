package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/fixture"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitrepo"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func isMountedOverlay(target string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == target && fields[2] == "fuse.workspace-overlay" {
			return true
		}
	}
	return false
}

func waitForMount(t *testing.T, target string, wantMounted bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mounted := isMountedOverlay(target)
		if mounted == wantMounted {
			if wantMounted {
				// Initialize reading on the mount target
				if err := selfReadable(target); err != nil {
					t.Fatalf("prepare %s for reading: %v", target, err)
				}
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mount %s: wantMounted=%v timed out", target, wantMounted)
}

func selfReadable(target string) error {
	fd, err := syscall.Open(filepath.Join(target, ".go-fuse-epoll-hack"), syscall.O_RDONLY, 0)
	if err != nil {
		return err
	}
	var files syscall.FdSet
	files.Bits[fd/64] |= 1 << (fd % 64)
	_, err = syscall.Select(fd+1, &files, nil, nil, &syscall.Timeval{})
	return errors.Join(err, syscall.Close(fd))
}

func TestE2ELocalWorkspace(t *testing.T) {
	// Locate dev-workspace directory relative to the repository root
	workspaceDir, err := filepath.Abs(filepath.Join("..", "..", "dev-workspace"))
	if err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(workspaceDir, config.Name)

	// Ensure the fixture workspace exists
	if _, err := fixture.Create(t.Context(), workspaceDir); err != nil {
		t.Fatalf("fixture.Create failed: %v", err)
	}

	alphaDir := filepath.Join(workspaceDir, "alpha")
	betaDir := filepath.Join(workspaceDir, "beta")
	workspaceOverlayFile := filepath.Join(workspaceDir, ".ai", "workspace", "AGENTS.md")

	// Save original overlay source file content to restore on cleanup
	origWorkspaceOverlay := scratch.Read(t, workspaceOverlayFile)

	// The fixture is reused across runs and keeps what its user changes, so
	// the commit and the branch this test adds to it are taken out again.
	head, err := gitrepo.Command(t.Context(), alphaDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("read the fixture's commit: %v", err)
	}
	startCommit := strings.TrimSpace(string(head))
	var branchName string

	// Setup cleanup to unmount and reset git state after test
	t.Cleanup(func() {
		// Stop any lingering mounts
		var stopOut, stopErr bytes.Buffer
		if err := runContext(context.Background(), []string{"overlay", "unmount", "--config", configFile}, &stopOut, &stopErr); err != nil {
			t.Errorf("unmount the fixture: %v: %s", err, stopErr.String())
		}
		scratch.Write(t, workspaceOverlayFile, string(origWorkspaceOverlay))
		// A restore that fails leaves the fixture changed for the next run.
		restore := func(dir string, args ...string) {
			if out, err := gitrepo.Command(context.Background(), dir, args...).CombinedOutput(); err != nil {
				t.Errorf("restore the fixture: git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
			}
		}
		restore(alphaDir, "checkout", "--force", "main")
		restore(alphaDir, "reset", "--hard", startCommit)
		if branchName != "" {
			restore(alphaDir, "branch", "-D", branchName)
		}
		restore(alphaDir, "clean", "-fd")
		restore(betaDir, "checkout", "--force", "main")
		restore(betaDir, "reset", "--hard", "HEAD")
		restore(betaDir, "clean", "-fd")
	})

	// 1. Mount overlay
	mountCtx, cancelMount := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var mountOut, mountErr bytes.Buffer
	go func() {
		done <- runContext(mountCtx, []string{"overlay", "mount", "--config", configFile, "--replace"}, &mountOut, &mountErr)
	}()

	t.Cleanup(func() {
		cancelMount()
		select {
		case <-done:
		default:
		}
	})

	waitForMount(t, alphaDir, true)
	waitForMount(t, betaDir, true)

	// 2. Status verification through CLI
	var statusOut, statusErr bytes.Buffer
	if err := runContext(t.Context(), []string{"overlay", "status", "--config", configFile}, &statusOut, &statusErr); err != nil {
		t.Fatalf("overlay status failed: %v: %s", err, statusErr.String())
	}
	if !strings.Contains(statusOut.String(), "fuse.workspace-overlay") {
		t.Fatalf("status output missing active mount: %q", statusOut.String())
	}

	// 3. Reading with markers & relative paths
	alphaAgents := filepath.Join(alphaDir, "AGENTS.md")
	contentBytes := scratch.Read(t, alphaAgents)
	content := string(contentBytes)

	if !strings.HasPrefix(content, "# Alpha project\n") {
		t.Errorf("expected project prefix in %q", content)
	}
	expectedMarker := "<!-- BEGIN WORKSPACE-OVERLAY: workspace (../.ai/workspace/AGENTS.md) -->"
	if !strings.Contains(content, expectedMarker) {
		t.Errorf("missing marker %q in %q", expectedMarker, content)
	}
	if !strings.Contains(content, "<!-- END WORKSPACE-OVERLAY: workspace -->") {
		t.Errorf("missing workspace end marker in %q", content)
	}
	if !strings.Contains(content, "<!-- BEGIN WORKSPACE-OVERLAY: alpha (../.ai/alpha/AGENTS.md) -->") {
		t.Errorf("missing alpha overlay marker in %q", content)
	}

	// 4. Git status is clean (caller-aware routing)
	cmd := gitrepo.Command(t.Context(), alphaDir, "status", "--porcelain")
	gitStatus, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status failed: %v: %s", err, gitStatus)
	}
	if len(strings.TrimSpace(string(gitStatus))) != 0 {
		t.Fatalf("git status is not clean while mounted: %q", string(gitStatus))
	}

	// 5. Two-way writes: edit project section
	updatedContent := strings.Replace(content, "# Alpha project", "# Alpha project\n- New E2E project rule", 1)
	if err := os.WriteFile(alphaAgents, []byte(updatedContent), 0644); err != nil {
		t.Fatalf("write to project section failed: %v", err)
	}

	// Git status should now detect modification in the project file
	gitStatus, _ = gitrepo.Command(t.Context(), alphaDir, "status", "--porcelain").CombinedOutput()
	if !strings.Contains(string(gitStatus), "AGENTS.md") {
		t.Fatalf("git status did not detect project modification: %q", string(gitStatus))
	}

	// Commit project edit and switch branch (proves Git works without EPERM)
	if out, err := gitrepo.Command(t.Context(), alphaDir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-am", "e2e: update project rules").CombinedOutput(); err != nil {
		t.Fatalf("git commit failed: %v: %s", err, out)
	}
	branchName = fmt.Sprintf("e2e-branch-%d", time.Now().UnixNano())
	if out, err := gitrepo.Command(t.Context(), alphaDir, "checkout", "-b", branchName).CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b failed: %v: %s", err, out)
	}

	// 6. Two-way writes: edit overlay section
	content = string(scratch.Read(t, alphaAgents))
	updatedContent = strings.Replace(content, "# Workspace overlay", "# Workspace overlay\n- New E2E workspace rule", 1)
	if err := os.WriteFile(alphaAgents, []byte(updatedContent), 0644); err != nil {
		t.Fatalf("write to workspace overlay section failed: %v", err)
	}

	// Verify the overlay source on disk was updated
	workspaceOverlayDisk := string(scratch.Read(t, workspaceOverlayFile))
	if !strings.Contains(workspaceOverlayDisk, "- New E2E workspace rule") {
		t.Fatalf("overlay source file not updated: %q", workspaceOverlayDisk)
	}

	// 7. Deletion lifecycle: rm AGENTS.md
	if err := os.Remove(alphaAgents); err != nil {
		t.Fatalf("first os.Remove failed: %v", err)
	}

	// Backing project copy should now be reported deleted by Git
	gitStatus, _ = gitrepo.Command(t.Context(), alphaDir, "status", "--porcelain").CombinedOutput()
	if !strings.Contains(string(gitStatus), "D AGENTS.md") {
		t.Fatalf("git status did not detect deletion: %q", string(gitStatus))
	}

	// But reading through the mount now falls back to overlay layers
	fallbackContent := string(scratch.Read(t, alphaAgents))
	if strings.Contains(fallbackContent, "# Alpha project") {
		t.Errorf("fallback content still contains deleted project rules: %q", fallbackContent)
	}
	if !strings.Contains(fallbackContent, "# Workspace overlay") {
		t.Errorf("fallback content missing workspace rules: %q", fallbackContent)
	}
	if !strings.Contains(fallbackContent, "<!-- BEGIN WORKSPACE-OVERLAY: alpha") {
		t.Errorf("fallback content missing alpha overlay rules: %q", fallbackContent)
	}

	// Second rm on overlay-only file must be rejected with EPERM
	if err := os.Remove(alphaAgents); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("second os.Remove on overlay returned %v, want EPERM", err)
	}

	// 8. Clean unmount via CLI
	var unmountOut, unmountErr bytes.Buffer
	if err := runContext(t.Context(), []string{"overlay", "unmount", "--config", configFile}, &unmountOut, &unmountErr); err != nil {
		t.Fatalf("unmount failed: %v: %s", err, unmountErr.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("mount returned error on exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mount did not exit after unmount")
	}

	waitForMount(t, alphaDir, false)
	waitForMount(t, betaDir, false)

	// Status confirms unmounted
	statusOut.Reset()
	statusErr.Reset()
	if err := runContext(t.Context(), []string{"overlay", "status", "--config", configFile}, &statusOut, &statusErr); err != nil {
		t.Fatalf("final status failed: %v", err)
	}
	if !strings.Contains(statusOut.String(), "unmounted") {
		t.Errorf("status after unmount = %q, want unmounted", statusOut.String())
	}
}
