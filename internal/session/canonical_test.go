package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"workspace-overlay/internal/config"
	"workspace-overlay/internal/overlayfs"
	"workspace-overlay/internal/scratch"
)

func TestDiscoverWorktreesIgnoresAliasOfTarget(t *testing.T) {
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(resolved, "real")
	scratch.GitRepo(t, filepath.Join(workspace, "project"))
	scratch.Write(t, filepath.Join(workspace, config.Name), "version=1\n[projects.project]\npath='project'\n")

	alias := filepath.Join(resolved, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}

	targets, err := discoverWorktrees(context.Background(), filepath.Join(alias, "project"), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Errorf("targets: got %d (expected 1 target, not 2 with duplicate alias)", len(targets))
	}
}

func TestSymlinkedWorkspaceMounts(t *testing.T) {
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(resolved, "real")
	scratch.GitRepo(t, filepath.Join(workspace, "project"))
	scratch.Write(t, filepath.Join(workspace, config.Name), "version=1\n[projects.project]\npath='project'\n")

	alias := filepath.Join(resolved, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(filepath.Join(alias, config.Name))
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := Selections(cfg, "", false, true)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel() })

	finished := make(chan error, 1)
	go func() { finished <- Mount(ctx, mounts) }()

	// Poll for mount every 50ms for up to 5s
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.Now().Add(5 * time.Second)

	for {
		select {
		case err := <-finished:
			t.Fatalf("mountSelections returned before mount appeared: %v (should say 'mount targets overlap' on old code)", err)
		case <-ticker.C:
			if time.Now().After(deadline) {
				cancel()
				t.Fatal("mount did not appear within 5s")
			}
			kind, err := mountedType(context.Background(), mounts[0].Target)
			if err == nil && kind == overlayfs.FilesystemType {
				// Mount succeeded, cleanup and exit
				cancel()
				select {
				case err := <-finished:
					if err != nil {
						t.Fatalf("unexpected error after cancel: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("mountSelections did not exit after context cancel")
				}
				waitMount(t, mounts[0].Target, "")
				return
			}
		}
	}
}

func TestMountedTargetErrorsNameTheCause(t *testing.T) {
	err := mountProjects(context.Background(), Selection{Root: "/", Project: "proc", Target: "/proc", Replace: true})
	if err == nil {
		t.Fatal("expected error for /proc mount")
	}
	if !strings.Contains(err.Error(), "refusing to replace") {
		t.Errorf("error message: got %q, want substring 'refusing to replace' (old code says 'requires --replace')", err.Error())
	}
}
