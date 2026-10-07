package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
	"workspace-overlay/internal/config"
	"workspace-overlay/internal/overlayfs"
	"workspace-overlay/internal/scratch"
)

func TestServeIntegration(t *testing.T) {
	v, targetDir, sharedDir, specDir := sessionView(t)

	// Write base and overlay files
	scratch.Write(t, filepath.Join(targetDir, "sample.txt"), "BASE-")
	scratch.Write(t, filepath.Join(specDir, "sample.txt"), "SPEC")

	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}

	plan := mountPlan{
		target:  targetDir,
		sources: []config.Source{{Name: "shared", Path: sharedDir, Glob: "**/*"}, {Name: "specific", Path: specDir, Glob: "**/*"}},
		view:    v,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serve(ctx, plan)
	}()

	// Wait for mount to become ready
	waitMount(t, targetDir, overlayfs.FilesystemType)

	// Read merged file through mountpoint
	data, err := os.ReadFile(filepath.Join(targetDir, "sample.txt"))
	if err != nil {
		t.Errorf("read merged file: %v", err)
	} else if string(data) != "BASE-SPEC" {
		t.Errorf("merged content = %q, want BASE-SPEC", string(data))
	}

	// Cancel context to unmount
	cancel()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("serve returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("timeout waiting for serve to exit after context cancel")
	}
}
