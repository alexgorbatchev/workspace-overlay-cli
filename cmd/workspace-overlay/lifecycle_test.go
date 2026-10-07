package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeIsolationMissingDirectories(t *testing.T) {
	root := t.TempDir()
	if err := ensureWorktreeIsolation(context.Background(), filepath.Join(root, "missing"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target: %v", err)
	}
	if err := ensureWorktreeIsolation(context.Background(), root, []overlaySource{{path: filepath.Join(root, "missing")}}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source: %v", err)
	}
}

func TestMountedType(t *testing.T) {
	ctx := context.Background()

	// 1. Unmounted directory returns empty string and no error
	tmpDir := t.TempDir()
	kind, err := mountedType(ctx, tmpDir)
	if err != nil {
		t.Fatalf("mountedType on unmounted dir error = %v", err)
	}
	if kind != "" {
		t.Errorf("mountedType on unmounted dir got %q, want empty", kind)
	}

	// 2. Mounted root directory "/" returns non-empty filesystem type
	rootKind, err := mountedType(ctx, "/")
	if err != nil {
		t.Fatalf("mountedType on / error = %v", err)
	}
	if rootKind == "" {
		t.Errorf("mountedType on / should not be empty")
	}

	// 3. Canceled context returns error
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = mountedType(canceledCtx, tmpDir)
	if err == nil {
		t.Errorf("mountedType with canceled context should return error")
	}
}

func TestUnmountOverlay(t *testing.T) {
	ctx := context.Background()

	// 1. Unmounting an unmounted directory succeeds (no-op)
	tmpDir := t.TempDir()
	if err := unmountOverlay(ctx, tmpDir); err != nil {
		t.Fatalf("unmountOverlay on unmounted dir error = %v", err)
	}

	// 2. Refuses to unmount a non-overlay filesystem like "/"
	err := unmountOverlay(ctx, "/")
	if err == nil || !strings.Contains(err.Error(), "refusing to unmount") {
		t.Errorf("expected error refusing to unmount non-overlay filesystem, got %v", err)
	}
}
