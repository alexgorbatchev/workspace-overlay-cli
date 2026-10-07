package overlayfs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"workspace-overlay/internal/scratch"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func helperSessionSetup(t *testing.T) (string, *node, context.Context) {
	t.Helper()
	root, _, _, specific := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(specific, "overlay/doc.md"), "old")
	if err := os.Mkdir(filepath.Join(specific, "overlay/keep"), 0755); err != nil {
		t.Fatal(err)
	}
	dir := &node{view: root.view, path: "overlay"}
	fs.NewNodeFS(dir, nil)
	return filepath.Join(specific, "overlay"), dir, context.Background()
}

func TestSessionCreatedEntriesAreRemovable(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		overlayPath, dir, ctx := helperSessionSetup(t)
		var out fuse.EntryOut

		// Create a file
		_, h, _, errno := dir.Create(ctx, "scratch.tmp", syscall.O_RDWR, 0644, &out)
		if errno != 0 {
			t.Fatalf("Create: %v", errno)
		}

		// Release the file handle
		if errno := h.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Fatalf("Release: %v", errno)
		}

		// Unlink the file
		if errno := dir.Unlink(ctx, "scratch.tmp"); errno != 0 {
			t.Fatalf("Unlink: %v", errno)
		}

		// Verify the file is gone
		if _, err := os.Lstat(filepath.Join(overlayPath, "scratch.tmp")); err == nil {
			t.Fatal("file should be gone")
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		overlayPath, dir, ctx := helperSessionSetup(t)
		var out fuse.EntryOut

		// Create a directory
		_, errno := dir.Mkdir(ctx, "newdir", 0755, &out)
		if errno != 0 {
			t.Fatalf("Mkdir: %v", errno)
		}

		// Remove the directory
		if errno := dir.Rmdir(ctx, "newdir"); errno != 0 {
			t.Fatalf("Rmdir: %v", errno)
		}

		// Verify the directory is gone
		if _, err := os.Lstat(filepath.Join(overlayPath, "newdir")); err == nil {
			t.Fatal("directory should be gone")
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		overlayPath, dir, ctx := helperSessionSetup(t)
		var out fuse.EntryOut

		// Create a symlink
		_, errno := dir.Symlink(ctx, "doc.md", "link", &out)
		if errno != 0 {
			t.Fatalf("Symlink: %v", errno)
		}

		// Unlink the symlink
		if errno := dir.Unlink(ctx, "link"); errno != 0 {
			t.Fatalf("Unlink: %v", errno)
		}

		// Verify the symlink is gone
		if _, err := os.Lstat(filepath.Join(overlayPath, "link")); err == nil {
			t.Fatal("symlink should be gone")
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	})
}

func TestSessionCreatedFileCanBeRenamedIntoPlace(t *testing.T) {
	overlayPath, dir, ctx := helperSessionSetup(t)
	var out fuse.EntryOut

	// Create a temp file
	_, h, _, errno := dir.Create(ctx, "new.md.tmp", syscall.O_RDWR, 0644, &out)
	if errno != 0 {
		t.Fatalf("Create: %v", errno)
	}

	// Write to the file
	if _, errno := h.(fs.FileWriter).Write(ctx, []byte("new\n"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}

	// Release the file
	if errno := h.(fs.FileReleaser).Release(ctx); errno != 0 {
		t.Fatalf("Release: %v", errno)
	}

	// Rename to the final name
	if errno := dir.Rename(ctx, "new.md.tmp", dir, "new.md", 0); errno != 0 {
		t.Fatalf("Rename: %v", errno)
	}

	// Verify content
	content := scratch.Read(t, filepath.Join(overlayPath, "new.md"))
	if string(content) != "new\n" {
		t.Fatalf("content: got %q, want %q", string(content), "new\n")
	}

	// The renamed file should still be removable (it's session-created)
	if errno := dir.Unlink(ctx, "new.md"); errno != 0 {
		t.Fatalf("Unlink renamed file: %v", errno)
	}

	// Verify it's gone
	if _, err := os.Lstat(filepath.Join(overlayPath, "new.md")); err == nil {
		t.Fatal("file should be gone")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestExistingOverlayEntriesStayProtected(t *testing.T) {
	overlayPath, dir, ctx := helperSessionSetup(t)

	// Try to delete pre-existing file
	if errno := dir.Unlink(ctx, "doc.md"); errno != syscall.EPERM {
		t.Fatalf("Unlink pre-existing file: got %v, want EPERM", errno)
	}

	// Try to delete pre-existing directory
	if errno := dir.Rmdir(ctx, "keep"); errno != syscall.EPERM {
		t.Fatalf("Rmdir pre-existing dir: got %v, want EPERM", errno)
	}

	// Try to rename pre-existing file
	if errno := dir.Rename(ctx, "doc.md", dir, "moved.md", 0); errno != syscall.EPERM {
		t.Fatalf("Rename pre-existing file: got %v, want EPERM", errno)
	}

	// Try to rename pre-existing directory
	if errno := dir.Rename(ctx, "keep", dir, "moved", 0); errno != syscall.EPERM {
		t.Fatalf("Rename pre-existing dir: got %v, want EPERM", errno)
	}

	// Verify doc.md still contains original content
	content := scratch.Read(t, filepath.Join(overlayPath, "doc.md"))
	if string(content) != "old" {
		t.Fatalf("file content changed: got %q, want %q", string(content), "old")
	}
}

func TestMountedOverlayDirectoryAcceptsScratchFiles(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "ai")
	scratch.Write(t, filepath.Join(source, ".agents/skills/demo/SKILL.md"), "skill\n")

	// Mount the project with the overlay source
	_ = mountedProject(t, project, source)

	// Wait for the mount to be ready

	dir := filepath.Join(project, ".agents/skills/demo")

	// Test 1: create and remove a file
	scratchFile := filepath.Join(dir, "scratch.tmp")
	if err := os.WriteFile(scratchFile, []byte("temp"), 0644); err != nil {
		t.Fatalf("WriteFile scratch: %v", err)
	}
	if err := os.Remove(scratchFile); err != nil {
		t.Fatalf("Remove scratch: %v", err)
	}

	// Test 2: create and remove a directory
	newDir := filepath.Join(dir, "newdir")
	if err := os.Mkdir(newDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Remove(newDir); err != nil {
		t.Fatalf("Remove newdir: %v", err)
	}

	// Test 3: create a file, then rename it
	tmpFile := filepath.Join(dir, "new.md.tmp")
	if err := os.WriteFile(tmpFile, []byte("new\n"), 0644); err != nil {
		t.Fatalf("WriteFile tmp: %v", err)
	}
	finalFile := filepath.Join(dir, "new.md")
	if err := os.Rename(tmpFile, finalFile); err != nil {
		t.Fatalf("Rename to new.md: %v", err)
	}
	if err := os.Remove(finalFile); err != nil {
		t.Fatalf("Remove new.md: %v", err)
	}

	// Verify that SKILL.md is still there and protected
	entries, err := os.ReadDir(filepath.Join(source, ".agents/skills/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "SKILL.md" {
		t.Fatalf("unexpected entries in source: %v", entries)
	}

	// Verify that SKILL.md cannot be deleted through the mount
	skillFile := filepath.Join(dir, "SKILL.md")
	if err := os.Remove(skillFile); err == nil {
		t.Fatal("SKILL.md should not be deletable")
	}
}
