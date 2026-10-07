package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
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

	_ = os.WriteFile(filepath.Join(baseDir, "base.txt"), []byte("base"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "spec.txt"), []byte("spec"), 0644)

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

	_ = os.WriteFile(filepath.Join(baseDir, "f0.txt"), []byte("0"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "f1.txt"), []byte("1"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "f2.txt"), []byte("2"), 0644)

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
	_ = os.WriteFile(filepath.Join(baseDir, "merged.txt"), []byte("BASE-"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "merged.txt"), []byte("SHARED"), 0644)

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

	// 1. Open directory returns EISDIR
	_, _, errno := root.Open(ctx, syscall.O_RDONLY)
	if errno != syscall.EISDIR {
		t.Errorf("Open on directory got errno %v, want EISDIR", errno)
	}

	// 2. Open single backing file returns loopback handle
	_ = os.WriteFile(filepath.Join(baseDir, "single.txt"), []byte("single"), 0644)
	childSingle := &node{view: root.view, path: "single.txt"}
	handle, flags, errno := childSingle.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open single file errno = %v", errno)
	}
	if flags != fuse.FOPEN_DIRECT_IO {
		t.Errorf("Open flags = %v, want FOPEN_DIRECT_IO", flags)
	}
	_ = handle.(fs.FileReleaser).Release(ctx)

	// 3. Open merged file returns mergedFile handle
	_ = os.WriteFile(filepath.Join(baseDir, "merged.txt"), []byte("1"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "merged.txt"), []byte("2"), 0644)
	childMerged := &node{view: root.view, path: "merged.txt"}
	handle, _, errno = childMerged.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open merged file errno = %v", errno)
	}
	if _, ok := handle.(*mergedFile); !ok {
		t.Errorf("expected *mergedFile handle, got %T", handle)
	}
	_ = handle.(fs.FileReleaser).Release(ctx)
}

func TestNodeReadlink(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	symlinkPath := filepath.Join(baseDir, "link.txt")
	_ = os.Symlink("target.txt", symlinkPath)

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

	_ = os.MkdirAll(filepath.Join(baseDir, ".git", "info"), 0755)
	_ = os.WriteFile(filepath.Join(baseDir, ".git", "info", "exclude"), []byte("# test"), 0644)

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
	_ = os.WriteFile(filepath.Join(baseDir, "merged_handle.txt"), []byte("1"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "merged_handle.txt"), []byte("2"), 0644)
	childMerged := &node{view: root.view, path: "merged_handle.txt"}
	handle, _, errno := childMerged.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer func() { _ = handle.(fs.FileReleaser).Release(ctx) }()

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

	// 1. Readlink on regular file fails
	_ = os.WriteFile(filepath.Join(baseDir, "not_a_link.txt"), []byte("reg"), 0644)
	regNode := &node{view: root.view, path: "not_a_link.txt"}
	_, errno := regNode.Readlink(ctx)
	if errno == 0 {
		t.Errorf("Readlink on regular file should return error")
	}

	// Readlink on nonexistent path fails
	missingNode := &node{view: root.view, path: "nonexistent_link.txt"}
	_, errno = missingNode.Readlink(ctx)
	if errno == 0 {
		t.Errorf("Readlink on nonexistent path should return error")
	}

	// 2. RefreshExclude error during Readdir and Getattr
	// Setup broken exclude
	brokenGe := &gitExclude{root: root.view.layers[0].root, marker: "test", block: []byte("tampered")}
	brokenView := &view{layers: root.view.layers, exclude: brokenGe}
	brokenRoot := &node{view: brokenView, path: "."}

	// Make an overlay file so refreshExclude tries to update
	_ = os.WriteFile(filepath.Join(baseDir, "..", "shared", "overlay_broken.txt"), []byte("x"), 0644)

	// In brokenView, update will fail because exclude file doesn't have the tampered block
	_, errno = brokenRoot.Readdir(ctx)
	if errno != syscall.EIO {
		t.Errorf("Readdir with broken exclude got errno %v, want EIO", errno)
	}

	var attrOut fuse.AttrOut
	errno = brokenRoot.Getattr(ctx, nil, &attrOut)
	if errno != syscall.EIO {
		t.Errorf("Getattr with broken exclude got errno %v, want EIO", errno)
	}
}
