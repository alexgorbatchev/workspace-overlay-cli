package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"workspace-overlay/internal/config"
)

func TestWorktreeIsolationMissingDirectories(t *testing.T) {
	root := t.TempDir()
	if err := ensureWorktreeIsolation(context.Background(), filepath.Join(root, "missing"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target: %v", err)
	}
	if err := ensureWorktreeIsolation(context.Background(), root, []config.Source{{Path: filepath.Join(root, "missing")}}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source: %v", err)
	}
}

func TestMountedType(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	t.Run("unmounted directory", func(t *testing.T) {
		kind, err := mountedType(ctx, tmpDir)
		if err != nil {
			t.Fatalf("mountedType on unmounted dir error = %v", err)
		}
		if kind != "" {
			t.Errorf("mountedType on unmounted dir got %q, want empty", kind)
		}
	})

	t.Run("root directory", func(t *testing.T) {
		rootKind, err := mountedType(ctx, "/")
		if err != nil {
			t.Fatalf("mountedType on / error = %v", err)
		}
		if rootKind == "" {
			t.Errorf("mountedType on / should not be empty")
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := mountedType(canceledCtx, tmpDir)
		if err == nil {
			t.Errorf("mountedType with canceled context should return error")
		}
	})
}

func TestUnmountOverlay(t *testing.T) {
	ctx := context.Background()

	t.Run("unmount unmounted directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		if err := unmountOverlay(ctx, tmpDir); err != nil {
			t.Fatalf("unmountOverlay on unmounted dir error = %v", err)
		}
	})

	t.Run("refuse to unmount non-overlay filesystem", func(t *testing.T) {
		err := unmountOverlay(ctx, "/")
		if err == nil || !strings.Contains(err.Error(), "refusing to unmount") {
			t.Errorf("expected error refusing to unmount non-overlay filesystem, got %v", err)
		}
	})
}

func TestMountInspectionNeedsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := mountedType(context.Background(), "."); err == nil {
		t.Fatal("mount status accepted removed working directory")
	}
	if err := unmountOverlay(context.Background(), "."); err == nil {
		t.Fatal("unmount accepted removed working directory")
	}
}
