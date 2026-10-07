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

func TestOverlayDeletionProtection(t *testing.T) {
	root, base, shared, specific := setupTestRootNode(t)
	ctx := context.Background()
	scratch.Write(t, filepath.Join(shared, "shared.txt"), "shared")
	scratch.Write(t, filepath.Join(base, "merged.txt"), "base")
	scratch.Write(t, filepath.Join(specific, "merged.txt"), "specific")
	if err := os.Mkdir(filepath.Join(specific, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shared.txt", "merged.txt"} {
		if errno := root.Unlink(ctx, name); errno != syscall.EPERM {
			t.Fatalf("delete overlay %s: %v", name, errno)
		}
		if errno := root.Rename(ctx, name, root, "moved.txt", 0); errno != syscall.EPERM {
			t.Fatalf("move overlay %s: %v", name, errno)
		}
	}
	if errno := root.Rmdir(ctx, "empty"); errno != syscall.EPERM {
		t.Fatalf("remove overlay directory: %v", errno)
	}
	if errno := root.Rename(ctx, "empty", root, "moved", 0); errno != syscall.EPERM {
		t.Fatalf("move overlay directory: %v", errno)
	}
	if got := string(scratch.Read(t, filepath.Join(specific, "merged.txt"))); got != "specific" {
		t.Fatalf("overlay contents changed: %s", got)
	}
}

func TestAtomicSaveInOverlayDirectory(t *testing.T) {
	root, _, _, specific := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(specific, "overlay/doc.md"), "old")
	dir := &node{view: root.view, path: "overlay"}
	fs.NewNodeFS(dir, nil)
	ctx := context.Background()
	var out fuse.EntryOut
	_, h, _, errno := dir.Create(ctx, "editor.tmp", syscall.O_RDWR, 0644, &out)
	if errno != 0 {
		t.Fatal(errno)
	}
	if _, errno := h.(fs.FileWriter).Write(ctx, []byte("updated"), 0); errno != 0 {
		t.Fatal(errno)
	}
	if errno := h.(fs.FileReleaser).Release(ctx); errno != 0 {
		t.Fatal(errno)
	}
	if errno := dir.Rename(ctx, "editor.tmp", dir, "doc.md", 0); errno != 0 {
		t.Fatalf("atomic editor save: %v", errno)
	}
	if got := string(scratch.Read(t, filepath.Join(specific, "overlay/doc.md"))); got != "updated" {
		t.Fatalf("saved content: %s", got)
	}
	if errno := dir.Rename(ctx, "doc.md", dir, "removed.md", 0); errno != syscall.EPERM {
		t.Fatalf("saved document became removable: %v", errno)
	}
}
