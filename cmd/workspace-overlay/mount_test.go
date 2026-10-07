package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServeIntegration(t *testing.T) {
	tmpDir := t.TempDir()
	targetDir := filepath.Join(tmpDir, "target")
	sharedDir := filepath.Join(tmpDir, "shared")
	specDir := filepath.Join(tmpDir, "spec")

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(specDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write base and overlay files
	_ = os.WriteFile(filepath.Join(targetDir, "sample.txt"), []byte("BASE-"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "sample.txt"), []byte("SPEC"), 0644)

	targetRoot, err := os.OpenRoot(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	sharedRoot, err := os.OpenRoot(sharedDir)
	if err != nil {
		t.Fatal(err)
	}
	specRoot, err := os.OpenRoot(specDir)
	if err != nil {
		t.Fatal(err)
	}

	v := &view{
		layers: []layer{
			{root: targetRoot},
			{root: sharedRoot},
			{root: specRoot},
		},
	}
	defer v.close()

	plan := mountPlan{
		target:  targetDir,
		sources: []overlaySource{{name: "shared", path: sharedDir, glob: "**/*"}, {name: "specific", path: specDir, glob: "**/*"}},
		view:    v,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serve(ctx, plan)
	}()

	// Wait for mount to become ready
	var mounted bool
	for range 20 {
		time.Sleep(50 * time.Millisecond)
		kind, err := mountedType(context.Background(), targetDir)
		if err == nil && kind == overlayType {
			mounted = true
			break
		}
	}

	if !mounted {
		cancel()
		t.Skip("FUSE mount not available in this test environment or took too long to mount")
		return
	}

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
