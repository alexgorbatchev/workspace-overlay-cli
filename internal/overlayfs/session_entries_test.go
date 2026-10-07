package overlayfs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
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
		_, h, _, errno := dir.Create(ctx, "scratch.tmp", syscall.O_RDWR, 0644, &out)
		if errno != 0 {
			t.Fatalf("Create: %v", errno)
		}

		if errno := h.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Fatalf("Release: %v", errno)
		}
		if errno := dir.Unlink(ctx, "scratch.tmp"); errno != 0 {
			t.Fatalf("Unlink: %v", errno)
		}

		if _, err := os.Lstat(filepath.Join(overlayPath, "scratch.tmp")); err == nil {
			t.Fatal("file should be gone")
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		overlayPath, dir, ctx := helperSessionSetup(t)
		var out fuse.EntryOut
		_, errno := dir.Mkdir(ctx, "newdir", 0755, &out)
		if errno != 0 {
			t.Fatalf("Mkdir: %v", errno)
		}

		if errno := dir.Rmdir(ctx, "newdir"); errno != 0 {
			t.Fatalf("Rmdir: %v", errno)
		}

		if _, err := os.Lstat(filepath.Join(overlayPath, "newdir")); err == nil {
			t.Fatal("directory should be gone")
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		overlayPath, dir, ctx := helperSessionSetup(t)
		var out fuse.EntryOut
		_, errno := dir.Symlink(ctx, "doc.md", "link", &out)
		if errno != 0 {
			t.Fatalf("Symlink: %v", errno)
		}

		if errno := dir.Unlink(ctx, "link"); errno != 0 {
			t.Fatalf("Unlink: %v", errno)
		}

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
	_, h, _, errno := dir.Create(ctx, "new.md.tmp", syscall.O_RDWR, 0644, &out)
	if errno != 0 {
		t.Fatalf("Create: %v", errno)
	}
	if _, errno := h.(fs.FileWriter).Write(ctx, []byte("new\n"), 0); errno != 0 {
		t.Fatalf("Write: %v", errno)
	}
	if errno := h.(fs.FileReleaser).Release(ctx); errno != 0 {
		t.Fatalf("Release: %v", errno)
	}

	if errno := dir.Rename(ctx, "new.md.tmp", dir, "new.md", 0); errno != 0 {
		t.Fatalf("Rename: %v", errno)
	}

	content := scratch.Read(t, filepath.Join(overlayPath, "new.md"))
	if string(content) != "new\n" {
		t.Fatalf("content: got %q, want %q", string(content), "new\n")
	}

	if errno := dir.Unlink(ctx, "new.md"); errno != 0 {
		t.Fatalf("Unlink renamed file: %v", errno)
	}
	if _, err := os.Lstat(filepath.Join(overlayPath, "new.md")); err == nil {
		t.Fatal("file should be gone")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestExistingOverlayEntriesStayProtected(t *testing.T) {
	overlayPath, dir, ctx := helperSessionSetup(t)
	if errno := dir.Unlink(ctx, "doc.md"); errno != syscall.EPERM {
		t.Fatalf("Unlink pre-existing file: got %v, want EPERM", errno)
	}
	if errno := dir.Rmdir(ctx, "keep"); errno != syscall.EPERM {
		t.Fatalf("Rmdir pre-existing dir: got %v, want EPERM", errno)
	}
	if errno := dir.Rename(ctx, "doc.md", dir, "moved.md", 0); errno != syscall.EPERM {
		t.Fatalf("Rename pre-existing file: got %v, want EPERM", errno)
	}
	if errno := dir.Rename(ctx, "keep", dir, "moved", 0); errno != syscall.EPERM {
		t.Fatalf("Rename pre-existing dir: got %v, want EPERM", errno)
	}
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
	_ = mountedProject(t, project, source)
	dir := filepath.Join(project, ".agents/skills/demo")

	scratchFile := filepath.Join(dir, "scratch.tmp")
	if err := os.WriteFile(scratchFile, []byte("temp"), 0644); err != nil {
		t.Fatalf("WriteFile scratch: %v", err)
	}
	if err := os.Remove(scratchFile); err != nil {
		t.Fatalf("Remove scratch: %v", err)
	}

	newDir := filepath.Join(dir, "newdir")
	if err := os.Mkdir(newDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Remove(newDir); err != nil {
		t.Fatalf("Remove newdir: %v", err)
	}

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

	entries, err := os.ReadDir(filepath.Join(source, ".agents/skills/demo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "SKILL.md" {
		t.Fatalf("unexpected entries in source: %v", entries)
	}

	skillFile := filepath.Join(dir, "SKILL.md")
	if err := os.Remove(skillFile); err == nil {
		t.Fatal("SKILL.md should not be deletable")
	}
}
