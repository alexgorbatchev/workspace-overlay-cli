package session

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"workspace-overlay/internal/config"
	"workspace-overlay/internal/overlayfs"
	"workspace-overlay/internal/registry"
	"workspace-overlay/internal/scratch"
)

func TestOwnerProcessHelper(t *testing.T) {
	configEnv := os.Getenv("WORKSPACE_OVERLAY_OWNER_CONFIG")
	if configEnv == "" {
		return
	}
	cfg, err := config.Load(configEnv)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := Selections(cfg, "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	// Serves until the parent test kills this process.
	if err := Mount(context.Background(), owned); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceRecoversDeadOwner(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)

	// Create a linked worktree
	workspaceDir := filepath.Join(root, ".workspaces", "one", "project")
	cmd := exec.Command("git", "-C", project, "worktree", "add", "-b", "linked", workspaceDir, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add worktree: %v: %s", err, out)
	}

	// Create overlay source file
	aiDir := filepath.Join(root, "ai")
	overlayFile := filepath.Join(aiDir, "overlay-only.md")
	scratch.Write(t, overlayFile, "overlay\n")

	// Write config
	configFile := filepath.Join(root, config.Name)
	scratch.Write(t, configFile, `version=1
[projects.project]
path='project'
[[overlays]]
name='ai'
source='ai'
projects=['*']
`)

	// Start the owner process
	cmd = exec.Command(os.Args[0], "-test.run=^TestOwnerProcessHelper$")
	cmd.Env = append(os.Environ(), "WORKSPACE_OVERLAY_OWNER_CONFIG="+configFile)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start owner process: %v", err)
	}

	// Wait for both mounts to be established
	waitMount(t, project, overlayfs.FilesystemType)
	waitMount(t, workspaceDir, overlayfs.FilesystemType)

	// Verify the mount is established before killing
	kind, err := mountedType(context.Background(), project)
	if err != nil {
		t.Fatalf("check mount type: %v", err)
	}
	if kind != overlayfs.FilesystemType {
		t.Fatalf("expected mount type %s, got %s", overlayfs.FilesystemType, kind)
	}

	// Kill the owner process
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill process: %v", err)
	}
	_ = cmd.Wait() // error is expected

	// Precondition: mount should still show as mounted but be dead
	kind, err = mountedType(context.Background(), project)
	if err != nil {
		t.Fatalf("check dead mount type: %v", err)
	}
	if kind != overlayfs.FilesystemType {
		t.Fatalf("expected dead mount still visible as %s, got %s", overlayfs.FilesystemType, kind)
	}

	// Verify accessing the mount fails with ENOTCONN
	_, err = os.Lstat(filepath.Join(project, ".git"))
	if err == nil {
		t.Fatal("expected error accessing dead mount, got nil")
	}

	// Register cleanup
	t.Cleanup(func() {
		for _, target := range []string{project, workspaceDir} {
			err := unmountOverlay(context.Background(), target)
			if err != nil {
				t.Logf("cleanup unmount error: %v", err)
			}
		}
	})

	// Test recovery under mount --replace
	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	mounts, err := Selections(cfg, "", true, true)
	if err != nil {
		t.Fatalf("get selections: %v", err)
	}

	// Run mountSelections with a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		finished <- Mount(ctx, mounts)
	}()

	// Poll for successful mount within 10 seconds
	deadline := time.Now().Add(10 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	success := false
	for time.Now().Before(deadline) {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatalf("mountSelections returned error: %v", err)
			}
			success = true
		case <-ticker.C:
			// Try to read the overlay file
			data, err := os.ReadFile(filepath.Join(project, "overlay-only.md"))
			if err == nil && string(data) == "overlay\n" {
				// Also check the worktree
				data2, err2 := os.ReadFile(filepath.Join(workspaceDir, "overlay-only.md"))
				if err2 == nil && string(data2) == "overlay\n" {
					success = true
				}
			}
			lastErr = err
		}
		if success {
			break
		}
	}

	if !success {
		output := output.String()
		t.Logf("owner process output:\n%s", output)
		if lastErr != nil {
			t.Logf("last read error: %v", lastErr)
		}
		cancel()
		t.Fatal("mount recovery did not succeed within 10 seconds")
	}

	// Cancel context and wait for goroutine to exit
	cancel()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("mountSelections did not exit within timeout after context cancel")
	}

	// Verify mounts are now clean
	waitMount(t, project, "")
	waitMount(t, workspaceDir, "")

	// Verify registry was cleaned
	state, err := registry.Read(root, "project")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if state.Version != 0 || len(state.Mounts) != 0 {
		t.Fatalf("registry not cleaned: %+v", state)
	}
}

func TestOwnerClearsLeftoverStopRequest(t *testing.T) {
	root := t.TempDir()
	dir, _, stop, err := registry.Paths(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	// Create a leftover stop file
	if err := os.WriteFile(stop, nil, 0600); err != nil {
		t.Fatal(err)
	}

	// Verify stop file exists
	if _, err := os.Stat(stop); err != nil {
		t.Fatalf("stop file not created: %v", err)
	}

	// registry.Open should succeed and clear the stop file
	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatalf("registry.Open failed: %v", err)
	}

	// Stop file should now be gone
	if _, err := os.Stat(stop); !os.IsNotExist(err) {
		if err != nil {
			t.Fatalf("stat stop file: %v", err)
		}
		t.Fatal("stop file was not removed by registry.Open")
	}

	// Close the registry
	if err := r.Close(); err != nil {
		t.Fatalf("Close registry: %v", err)
	}
}

func TestStopRequestRemovedOnceOwnerIsGone(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "project")

	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}

	// Record the target mount
	if err := r.Record(target, "", nil); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Run stopRegistered in a goroutine
	finished := make(chan error, 1)
	go func() {
		finished <- stopRegistered(context.Background(), root, "project")
	}()

	// Poll until stop file is created
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()

	stopExists := false
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(r.StopFile()); err == nil {
				stopExists = true
				break
			}
		case <-deadline.C:
			t.Fatal("stop file was not created")
		}
		if stopExists {
			break
		}
	}

	// Now remove the registry file to model an owner that cleaned up but left the stop file
	if err := os.Remove(r.File()); err != nil {
		t.Fatalf("remove registry file: %v", err)
	}

	// Wait for stopRegistered to finish (should succeed and remove stop file)
	var stopErr error
	select {
	case stopErr = <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("stopRegistered did not complete within 2 seconds")
	}

	if stopErr != nil {
		t.Fatalf("stopRegistered returned error: %v", stopErr)
	}

	// Verify stop file is now gone
	if _, err := os.Stat(r.StopFile()); !os.IsNotExist(err) {
		if err != nil {
			t.Fatalf("stat stop file: %v", err)
		}
		t.Fatal("stop file was not removed after owner gone")
	}

	// Cleanup
	if err := r.Forget(target); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
