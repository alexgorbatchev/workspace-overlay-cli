package overlayfs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitexclude"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func setupTestRootNode(t *testing.T) (*node, string, string, string) {
	t.Helper()
	v, baseDir, sharedDir, specDir := setupTestView(t)
	root := &node{view: v, path: "."}
	fs.NewNodeFS(root, nil)
	return root, baseDir, sharedDir, specDir
}

func TestNodeLookup(t *testing.T) {
	root, baseDir, _, specDir := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "base.txt"), "base")
	scratch.Write(t, filepath.Join(specDir, "spec.txt"), "spec")

	var entryOut fuse.EntryOut

	// Lookup base file
	inode, errno := root.Lookup(ctx, "base.txt", &entryOut)
	if errno != 0 || inode == nil {
		t.Fatalf("Lookup base.txt errno = %v", errno)
	}
	if entryOut.Attr.Mode&syscall.S_IFREG == 0 {
		t.Errorf("expected regular file mode in entryOut")
	}

	// Lookup overlay file
	inode, errno = root.Lookup(ctx, "spec.txt", &entryOut)
	if errno != 0 || inode == nil {
		t.Fatalf("Lookup spec.txt errno = %v", errno)
	}

	// Lookup non-existent file
	_, errno = root.Lookup(ctx, "nonexistent.txt", &entryOut)
	if errno != syscall.ENOENT {
		t.Errorf("Lookup nonexistent got errno %v, want ENOENT", errno)
	}
}

func TestNodeReaddir(t *testing.T) {
	root, baseDir, sharedDir, specDir := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "f0.txt"), "0")
	scratch.Write(t, filepath.Join(sharedDir, "f1.txt"), "1")
	scratch.Write(t, filepath.Join(specDir, "f2.txt"), "2")

	stream, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir errno = %v", errno)
	}

	names := make(map[string]bool)
	for stream.HasNext() {
		entry, errno := stream.Next()
		if errno != 0 {
			t.Fatalf("stream.Next errno = %v", errno)
		}
		names[entry.Name] = true
	}

	for _, want := range []string{"f0.txt", "f1.txt", "f2.txt"} {
		if !names[want] {
			t.Errorf("Readdir missing expected file %q", want)
		}
	}
}

func TestNodeGetattr(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Getattr on root directory
	var attrOut fuse.AttrOut
	if errno := root.Getattr(ctx, nil, &attrOut); errno != 0 {
		t.Fatalf("Getattr on root errno = %v", errno)
	}
	if attrOut.Mode&syscall.S_IFDIR == 0 {
		t.Errorf("expected root to be a directory")
	}

	// Getattr on merged file
	scratch.Write(t, filepath.Join(baseDir, "merged.txt"), "BASE-")
	scratch.Write(t, filepath.Join(sharedDir, "merged.txt"), "SHARED")

	child := &node{view: root.view, path: "merged.txt"}
	if errno := child.Getattr(ctx, nil, &attrOut); errno != 0 {
		t.Fatalf("Getattr on merged.txt errno = %v", errno)
	}
	if attrOut.Size != uint64(len("BASE-SHARED")) {
		t.Errorf("Getattr size on merged.txt = %d, want %d", attrOut.Size, len("BASE-SHARED"))
	}
}

func TestNodeOpen(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	t.Run("directory returns EISDIR", func(t *testing.T) {
		_, _, errno := root.Open(ctx, syscall.O_RDONLY)
		if errno != syscall.EISDIR {
			t.Errorf("Open on directory got errno %v, want EISDIR", errno)
		}
	})

	t.Run("single backing file returns loopback handle", func(t *testing.T) {
		scratch.Write(t, filepath.Join(baseDir, "single.txt"), "single")
		childSingle := &node{view: root.view, path: "single.txt"}
		handle, flags, errno := childSingle.Open(ctx, syscall.O_RDONLY)
		if errno != 0 {
			t.Fatalf("Open single file errno = %v", errno)
		}
		if flags != 0 {
			t.Errorf("Open flags = %v, want 0", flags)
		}
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Fatalf("Release single file errno = %v", errno)
		}
	})

	t.Run("merged file returns mergedFile handle", func(t *testing.T) {
		scratch.Write(t, filepath.Join(baseDir, "merged.txt"), "1")
		scratch.Write(t, filepath.Join(sharedDir, "merged.txt"), "2")
		childMerged := &node{view: root.view, path: "merged.txt"}
		handle, _, errno := childMerged.Open(ctx, syscall.O_RDONLY)
		if errno != 0 {
			t.Fatalf("Open merged file errno = %v", errno)
		}
		if _, ok := handle.(*mergedFile); !ok {
			t.Errorf("expected *mergedFile handle, got %T", handle)
		}
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Fatalf("Release merged file errno = %v", errno)
		}
	})
}

func TestNodeReadlink(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	symlinkPath := filepath.Join(baseDir, "link.txt")
	if err := os.Symlink("target.txt", symlinkPath); err != nil {
		t.Fatal(err)
	}

	child := &node{view: root.view, path: "link.txt"}
	target, errno := child.Readlink(ctx)
	if errno != 0 {
		t.Fatalf("Readlink errno = %v", errno)
	}
	if string(target) != "target.txt" {
		t.Errorf("Readlink got %q, want %q", string(target), "target.txt")
	}
}

func TestNodeGetattrHandleAndGitPaths(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Mkdir(t, filepath.Join(baseDir, ".git", "info"))
	scratch.Write(t, filepath.Join(baseDir, ".git", "info", "exclude"), "# test")

	var attrOut fuse.AttrOut
	gitNode := &node{view: root.view, path: ".git"}
	if errno := gitNode.Getattr(ctx, nil, &attrOut); errno != 0 {
		t.Errorf("Getattr on .git errno = %v", errno)
	}

	excludeNode := &node{view: root.view, path: ".git/info/exclude"}
	if errno := excludeNode.Getattr(ctx, nil, &attrOut); errno != 0 {
		t.Errorf("Getattr on .git/info/exclude errno = %v", errno)
	}

	// Test Getattr with FileGetattrer handle
	scratch.Write(t, filepath.Join(baseDir, "merged_handle.txt"), "1")
	scratch.Write(t, filepath.Join(sharedDir, "merged_handle.txt"), "2")
	childMerged := &node{view: root.view, path: "merged_handle.txt"}
	handle, _, errno := childMerged.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Errorf("Release handle errno = %v", errno)
		}
	}()

	if errno := childMerged.Getattr(ctx, handle, &attrOut); errno != 0 {
		t.Errorf("Getattr with handle errno = %v", errno)
	}
}

func TestNodeOpenNonRegular(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	fifoPath := filepath.Join(baseDir, "my_fifo")
	if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
		t.Skip("mkfifo not supported")
	}

	childFifo := &node{view: root.view, path: "my_fifo"}
	_, _, errno := childFifo.Open(ctx, syscall.O_RDONLY)
	if errno != syscall.ENOTSUP {
		t.Errorf("Open on FIFO got errno %v, want ENOTSUP", errno)
	}
}

func TestNodeErrors(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	t.Run("Readlink on regular file and nonexistent path", func(t *testing.T) {
		scratch.Write(t, filepath.Join(baseDir, "not_a_link.txt"), "reg")
		regNode := &node{view: root.view, path: "not_a_link.txt"}
		_, errno := regNode.Readlink(ctx)
		if errno == 0 {
			t.Errorf("Readlink on regular file should return error")
		}

		missingNode := &node{view: root.view, path: "nonexistent_link.txt"}
		_, errno = missingNode.Readlink(ctx)
		if errno == 0 {
			t.Errorf("Readlink on nonexistent path should return error")
		}
	})

	t.Run("RefreshExclude error during Readdir and Getattr", func(t *testing.T) {
		// Initialize baseDir as a git repository
		scratch.GitRepo(t, baseDir)

		// Build broken state through the public API
		ge, err := gitexclude.Open(ctx, baseDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := ge.Update([]string{"/test.txt"}); err != nil {
			t.Fatal(err)
		}

		// Overwrite the managed block on disk to model a corrupted exclude file
		excludePath := filepath.Join(baseDir, ".git", "info", "exclude")
		scratch.Write(t, excludePath, "unmanaged content")

		brokenView := &View{layers: root.view.layers, exclude: ge}
		brokenRoot := &node{view: brokenView, path: "."}

		scratch.Write(t, filepath.Join(baseDir, "..", "shared", "overlay_broken.txt"), "x")

		_, errno := brokenRoot.Readdir(ctx)
		if errno != syscall.EIO {
			t.Errorf("Readdir with broken exclude got errno %v, want EIO", errno)
		}

		var attrOut fuse.AttrOut
		errno = brokenRoot.Getattr(ctx, nil, &attrOut)
		if errno != syscall.EIO {
			t.Errorf("Getattr with broken exclude got errno %v, want EIO", errno)
		}

		// Cleanup
		ge.Close()
	})
}
