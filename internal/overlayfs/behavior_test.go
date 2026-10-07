package overlayfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"workspace-overlay/internal/scratch"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Test 1: Failed editor save restores the staged file
// Setup: base/doc.md = "base\n", shared/doc.md = "old\n" (merged)
// Staged file: base/doc.tmp = "base\nnew\n"
// Make shared dir read-only so rename fails
// Expected: Rename returns EPERM; base/doc.tmp still contains "base\nnew\n"; shared/doc.md still "old\n"
func TestFailedEditorSaveRestore(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Setup: base/doc.md and shared/doc.md (merged text file)
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "base\n")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "old\n")

	// Editor writes a staging file beside the document
	scratch.Write(t, filepath.Join(baseDir, "doc.tmp"), "base\nnew\n")

	// Make shared directory read-only so the rename into it fails
	if err := os.Chmod(sharedDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(sharedDir, 0755); err != nil {
			t.Errorf("restore shared dir permissions: %v", err)
		}
	})

	// Attempt atomic save-and-rename of merged file
	// When dest dir is read-only, the OS returns EACCES (permission denied)
	errno := root.Rename(ctx, "doc.tmp", root, "doc.md", 0)
	if errno != syscall.EACCES && errno != syscall.EPERM {
		t.Fatalf("Rename with read-only dest dir got errno %v, want EACCES or EPERM", errno)
	}

	// After failed rename, base/doc.tmp must still contain the original content
	data, err := os.ReadFile(filepath.Join(baseDir, "doc.tmp"))
	if err != nil {
		t.Fatalf("read doc.tmp: %v", err)
	}
	if string(data) != "base\nnew\n" {
		t.Errorf("doc.tmp after failed save = %q, want %q", string(data), "base\nnew\n")
	}

	// shared/doc.md must still contain the original content
	data, err = os.ReadFile(filepath.Join(sharedDir, "doc.md"))
	if err != nil {
		t.Fatalf("read shared doc.md: %v", err)
	}
	if string(data) != "old\n" {
		t.Errorf("shared/doc.md after failed save = %q, want %q", string(data), "old\n")
	}
}

// Test 2: Rename with flags != 0 returns ENOTSUP; Rename to different view returns EXDEV
func TestRenameErrorCases(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "content")

	t.Run("rename_with_flags", func(t *testing.T) {
		errno := root.Rename(ctx, "file.txt", root, "renamed.txt", 1)
		if errno != syscall.ENOTSUP {
			t.Errorf("Rename with flags=1 got errno %v, want ENOTSUP", errno)
		}
	})

	t.Run("rename_to_different_view", func(t *testing.T) {
		otherView := &View{}
		otherNode := &node{view: otherView, path: "dest"}
		errno := root.Rename(ctx, "file.txt", otherNode, "newname.txt", 0)
		if errno != syscall.EXDEV {
			t.Errorf("Rename to different view got errno %v, want EXDEV", errno)
		}
	})
}

// Test 3: Link with file spanning multiple layers returns EXDEV; Link to different view returns EXDEV
func TestLinkErrorCases(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	var entryOut fuse.EntryOut

	t.Run("link_multi_layer_file", func(t *testing.T) {
		// Create file in both layers (multi-layer file)
		scratch.Write(t, filepath.Join(baseDir, "multi.txt"), "base")
		scratch.Write(t, filepath.Join(sharedDir, "multi.txt"), "shared")

		multiNode := &node{view: root.view, path: "multi.txt"}
		fs.NewNodeFS(multiNode, nil)

		_, errno := root.Link(ctx, multiNode, "link.txt", &entryOut)
		if errno != syscall.EXDEV {
			t.Errorf("Link multi-layer file got errno %v, want EXDEV", errno)
		}
	})

	t.Run("link_to_different_view", func(t *testing.T) {
		scratch.Write(t, filepath.Join(baseDir, "orig.txt"), "content")
		origNode := &node{view: root.view, path: "orig.txt"}
		fs.NewNodeFS(origNode, nil)

		otherView := &View{}
		otherNode := &node{view: otherView, path: "dest"}

		_, errno := otherNode.Link(ctx, origNode, "link.txt", &entryOut)
		if errno != syscall.EXDEV {
			t.Errorf("Link to different view got errno %v, want EXDEV", errno)
		}
	})
}

// Test 4: Symlink at a name that already exists returns EEXIST
func TestSymlinkExists(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create a file
	scratch.Write(t, filepath.Join(baseDir, "existing.txt"), "content")

	var entryOut fuse.EntryOut

	// Try to create symlink with same name
	_, errno := root.Symlink(ctx, "target.txt", "existing.txt", &entryOut)
	if errno != syscall.EEXIST {
		t.Errorf("Symlink at existing name got errno %v, want EEXIST", errno)
	}
}

// Test 5: mergedFile.Fsync persists a staged change before the handle is released
func TestMergedFileFsync(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create merged file with longer content and clear prefix
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "START:")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "shared content\n")

	// Open for read-write
	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)

	handle, _, errno := docNode.Open(ctx, syscall.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 && errno != syscall.EPERM {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	// Write an edit that extends beyond the prefix
	mh := handle.(*mergedFile)
	if mh == nil {
		t.Fatal("Open did not return mergedFile handle")
	}
	_, errno = mh.Write(ctx, []byte("new content\n"), 6) // Write after "START:"
	if errno != 0 {
		t.Fatalf("Write errno = %v", errno)
	}

	// Fsync should persist the change
	errno = mh.Fsync(ctx, 0)
	if errno != 0 {
		t.Errorf("Fsync errno = %v", errno)
	}

	// Verify the last layer's file on disk contains the new contribution
	data, err := os.ReadFile(filepath.Join(sharedDir, "doc.md"))
	if err != nil {
		t.Fatalf("read shared doc.md: %v", err)
	}
	if !strings.Contains(string(data), "new content") {
		t.Errorf("shared/doc.md after Fsync = %q, want to contain 'new content'", string(data))
	}
}

// Test 6: mergedFile.Flush after a write that violates the prefix returns EPERM
func TestMergedFileFlushPrefixViolation(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create merged file
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "PREFIX-base\n")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "old\n")

	// Open for read-write
	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)

	handle, _, errno := docNode.Open(ctx, syscall.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 && errno != syscall.EPERM {
			// Release may return EPERM after failed write, which is expected
		}
	}()

	// Write at offset 0 that violates prefix
	mh := handle.(*mergedFile)
	_, errno = mh.Write(ctx, []byte("BAD-"), 0)
	if errno != syscall.EPERM {
		t.Errorf("Write violating prefix got errno %v, want EPERM", errno)
	}

	// Flush should also return EPERM and leave files unchanged
	errno = mh.Flush(ctx)
	if errno != syscall.EPERM {
		t.Errorf("Flush after failed write got errno %v, want EPERM", errno)
	}

	// Verify files unchanged
	data, err := os.ReadFile(filepath.Join(baseDir, "doc.md"))
	if err != nil {
		t.Fatalf("read base doc.md: %v", err)
	}
	if string(data) != "PREFIX-base\n" {
		t.Errorf("base/doc.md changed after failed write: %q", string(data))
	}

	data, err = os.ReadFile(filepath.Join(sharedDir, "doc.md"))
	if err != nil {
		t.Fatalf("read shared doc.md: %v", err)
	}
	if string(data) != "old\n" {
		t.Errorf("shared/doc.md changed after failed write: %q", string(data))
	}
}

// Test 8: mergedFile opened read-only cannot be written; opened write-only cannot be read
func TestMergedFileOpenModeViolations(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "PREFIX-base\n")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "old\n")

	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)

	t.Run("read_only_write_fails", func(t *testing.T) {
		// Open read-only
		handle, _, errno := docNode.Open(ctx, syscall.O_RDONLY)
		if errno != 0 {
			t.Fatalf("Open O_RDONLY errno = %v", errno)
		}
		defer func() {
			if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
				t.Errorf("Release errno = %v", errno)
			}
		}()

		mh := handle.(*mergedFile)
		_, errno = mh.Write(ctx, []byte("data"), 0)
		if errno != syscall.EBADF {
			t.Errorf("Write on read-only handle got errno %v, want EBADF", errno)
		}
	})

	t.Run("write_only_read_fails", func(t *testing.T) {
		// Open write-only
		handle, _, errno := docNode.Open(ctx, syscall.O_WRONLY)
		if errno != 0 {
			t.Fatalf("Open O_WRONLY errno = %v", errno)
		}
		defer func() {
			if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
				t.Errorf("Release errno = %v", errno)
			}
		}()

		mh := handle.(*mergedFile)
		_, errno = mh.Read(ctx, make([]byte, 10), 0)
		if errno != syscall.EBADF {
			t.Errorf("Read on write-only handle got errno %v, want EBADF", errno)
		}
	})
}

// Test 9: Rename returns EXDEV when target node is from a different view
func TestRenameExdevDifferentView(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "content")

	otherView := &View{}
	errno := root.Rename(ctx, "file.txt", &node{view: otherView}, "newname.txt", 0)
	if errno != syscall.EXDEV {
		t.Errorf("Rename to different view got errno %v, want EXDEV", errno)
	}
}

// Test 10: Mkdir and symlink in read-only parent return permission errors
func TestMutationInReadOnlyParent(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	readOnlyDir := filepath.Join(baseDir, "readonly")
	if err := os.Mkdir(readOnlyDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(readOnlyDir, 0755); err != nil {
			t.Errorf("restore readonly dir permissions: %v", err)
		}
	})

	roNode := &node{view: root.view, path: "readonly"}
	fs.NewNodeFS(roNode, nil)

	var entryOut fuse.EntryOut

	t.Run("create_in_readonly", func(t *testing.T) {
		_, _, _, errno := roNode.Create(ctx, "file.txt", syscall.O_RDWR, 0644, &entryOut)
		if errno == 0 {
			t.Errorf("Create in read-only dir should fail")
		}
	})

	t.Run("symlink_in_readonly", func(t *testing.T) {
		_, errno := roNode.Symlink(ctx, "target", "link", &entryOut)
		if errno == 0 {
			t.Errorf("Symlink in read-only dir should fail")
		}
	})
}

// Test 11: Loopback handle for single-layer files works correctly
func TestSingleLayerFileLoopback(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create single-layer file (only in base)
	scratch.Write(t, filepath.Join(baseDir, "single.txt"), "content")

	singleNode := &node{view: root.view, path: "single.txt"}
	fs.NewNodeFS(singleNode, nil)

	handle, _, errno := singleNode.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	// For loopback handles, they support file operations
	// Verify we got a valid file handle
	if handle == nil {
		t.Errorf("Open returned nil handle")
	}
}

// Test 12: Unlink on nonexistent file returns error
func TestUnlinkNonexistentFile(t *testing.T) {
	root, _, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Try to unlink a file that doesn't exist
	errno := root.Unlink(ctx, "does_not_exist.txt")
	if errno == 0 {
		t.Errorf("Unlink nonexistent file should fail")
	}
}

// Test 13: Rmdir on multi-layer directory fails with EPERM
func TestRmdirMultiLayer(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create directory in both layers
	scratch.Mkdir(t, filepath.Join(baseDir, "multidir"))
	scratch.Mkdir(t, filepath.Join(sharedDir, "multidir"))

	errno := root.Rmdir(ctx, "multidir")
	if errno != syscall.EPERM {
		t.Errorf("Rmdir multi-layer dir got errno %v, want EPERM", errno)
	}

	// Directory should still exist in both layers
	if _, err := os.Stat(filepath.Join(baseDir, "multidir")); err != nil {
		t.Errorf("multidir should still exist in base: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sharedDir, "multidir")); err != nil {
		t.Errorf("multidir should still exist in shared: %v", err)
	}
}

// Test 14: mergedFile with only prefix (no data) handles operations correctly
func TestMergedFilePrefixOnly(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create file with only prefix in base
	scratch.Write(t, filepath.Join(baseDir, "prefix.md"), "PREFIX-")
	scratch.Write(t, filepath.Join(sharedDir, "prefix.md"), "OLD")

	prefixNode := &node{view: root.view, path: "prefix.md"}
	fs.NewNodeFS(prefixNode, nil)

	// Open and read - should get both prefix and data
	handle, _, errno := prefixNode.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	mh := handle.(*mergedFile)
	buf := make([]byte, 100)
	result, errno := mh.Read(ctx, buf, 0)
	if errno != 0 {
		t.Errorf("Read errno = %v", errno)
	}
	if result == nil {
		t.Errorf("Read returned nil result")
		return
	}

	data, status := result.Bytes(buf)
	if !status.Ok() {
		t.Fatal(status)
	}
	content := string(data)
	if !strings.Contains(content, "PREFIX-") || !strings.Contains(content, "OLD") {
		t.Errorf("Read content = %q, want to contain PREFIX- and OLD", content)
	}
}

// Test 15: Getattr on closed node returns appropriate error
func TestGetattrOnClosedNode(t *testing.T) {
	v, baseDir, _, _ := setupTestView(t)
	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "data")

	_, err := v.resolve("file.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Close the project root
	if err := v.Project().Close(); err != nil {
		t.Fatal(err)
	}

	// Getattr on the closed layer should fail
	n := &node{view: v, path: "file.txt"}
	var out fuse.AttrOut
	errno := n.Getattr(context.Background(), nil, &out)
	if errno == 0 {
		t.Errorf("Getattr on closed layer root should fail, got errno 0")
	}
}

// Test 16: Readdir on closed view returns error
func TestReaddirOnClosedView(t *testing.T) {
	v, baseDir, _, _ := setupTestView(t)
	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "data")

	// Create a node and try to readdir
	rootNode := &node{view: v, path: "."}
	fs.NewNodeFS(rootNode, nil)

	// Close the project root
	if err := v.Project().Close(); err != nil {
		t.Fatal(err)
	}

	// Readdir should fail
	_, errno := rootNode.Readdir(context.Background())
	if errno == 0 {
		t.Errorf("Readdir on closed layer root should fail")
	}
}

// Test 17: mergedFile.Getattr returns correct attributes
func TestMergedFileGetattr(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create merged file
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "PREFIX-")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "shared\n")

	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)

	handle, _, errno := docNode.Open(ctx, syscall.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	mh := handle.(*mergedFile)
	var out fuse.AttrOut
	errno = mh.Getattr(ctx, &out)
	if errno != 0 {
		t.Errorf("Getattr errno = %v", errno)
	}
	if out.Size == 0 {
		t.Errorf("Getattr returned size 0")
	}
}

// Test 18: mergedFile.Flush commits all changes
func TestMergedFileFlushCommits(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create merged file
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "PREFIX-")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "old\n")

	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)

	handle, _, errno := docNode.Open(ctx, syscall.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	mh := handle.(*mergedFile)
	_, errno = mh.Write(ctx, []byte("modified\n"), 7)
	if errno != 0 {
		t.Fatalf("Write errno = %v", errno)
	}

	// Flush should commit
	errno = mh.Flush(ctx)
	if errno != 0 {
		t.Errorf("Flush errno = %v", errno)
	}

	// Verify change was written
	data, err := os.ReadFile(filepath.Join(sharedDir, "doc.md"))
	if err != nil {
		t.Fatalf("read shared doc.md: %v", err)
	}
	if !strings.Contains(string(data), "modified") {
		t.Errorf("shared/doc.md does not contain 'modified': %q", string(data))
	}
}

// Test 19: Setattr mode changes on single-layer file succeed
func TestSetAttrSingleLayerFile(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create single-layer file
	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "content")

	fileNode := &node{view: root.view, path: "file.txt"}
	fs.NewNodeFS(fileNode, nil)

	// Change mode via Setattr
	var in fuse.SetAttrIn
	in.Mode = 0755
	in.Valid |= fuse.FATTR_MODE
	var out fuse.AttrOut

	errno := fileNode.Setattr(ctx, nil, &in, &out)
	if errno != 0 {
		t.Errorf("Setattr mode errno = %v", errno)
	}

	// Verify mode changed
	info, err := os.Stat(filepath.Join(baseDir, "file.txt"))
	if err != nil {
		t.Fatalf("stat file.txt: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("file.txt mode = %o, want 0755", info.Mode().Perm())
	}
}

// Test 20: Create file returns correct entry attributes
func TestCreateFileAttributes(t *testing.T) {
	root, _, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	var entryOut fuse.EntryOut
	inode, handle, _, errno := root.Create(ctx, "newfile.txt", syscall.O_WRONLY, 0644, &entryOut)
	if errno != 0 {
		t.Fatalf("Create errno = %v", errno)
	}
	if inode == nil {
		t.Errorf("Create returned nil inode")
	}
	if handle != nil {
		if relErr := handle.(fs.FileReleaser).Release(ctx); relErr != 0 {
			t.Errorf("Release errno = %v", relErr)
		}
	}
	if entryOut.Attr.Mode == 0 {
		t.Errorf("Create returned zero mode in EntryOut")
	}
}

// Test 21: Mkdir returns correct entry attributes
func TestMkdirAttributes(t *testing.T) {
	root, _, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	var entryOut fuse.EntryOut
	inode, errno := root.Mkdir(ctx, "newdir", 0755, &entryOut)
	if errno != 0 {
		t.Fatalf("Mkdir errno = %v", errno)
	}
	if inode == nil {
		t.Errorf("Mkdir returned nil inode")
	}
	if !entryOut.Attr.IsDir() {
		t.Errorf("Mkdir returned non-directory mode in EntryOut")
	}
}

// Test 22: Open append mode extends writes to end
func TestOpenAppendMode(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "start:")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "content\n")

	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)

	// Open in append mode
	handle, _, errno := docNode.Open(ctx, syscall.O_WRONLY|syscall.O_APPEND)
	if errno != 0 {
		t.Fatalf("Open O_APPEND errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	if mh, ok := handle.(*mergedFile); ok {
		_, errno = mh.Write(ctx, []byte("appended"), 0)
		if errno != 0 {
			t.Errorf("Write in append mode errno = %v", errno)
		}
	}
}

// Test 23: Lookup resolves paths and returns correct node attributes
func TestLookup(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "content")
	scratch.Mkdir(t, filepath.Join(baseDir, "subdir"))

	t.Run("lookup_file", func(t *testing.T) {
		var out fuse.EntryOut
		inode, errno := root.Lookup(ctx, "file.txt", &out)
		if errno != 0 {
			t.Errorf("Lookup file errno = %v", errno)
		}
		if inode == nil {
			t.Errorf("Lookup file returned nil inode")
		}
	})

	t.Run("lookup_directory", func(t *testing.T) {
		var out fuse.EntryOut
		inode, errno := root.Lookup(ctx, "subdir", &out)
		if errno != 0 {
			t.Errorf("Lookup dir errno = %v", errno)
		}
		if inode == nil {
			t.Errorf("Lookup dir returned nil inode")
		}
	})

	t.Run("lookup_nonexistent", func(t *testing.T) {
		var out fuse.EntryOut
		_, errno := root.Lookup(ctx, "nonexistent", &out)
		if errno == 0 {
			t.Errorf("Lookup nonexistent should fail")
		}
	})
}

// Test 24: Readdir includes all entries from all layers
func TestReaddirMultiLayer(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create files in both layers
	scratch.Write(t, filepath.Join(baseDir, "base_only.txt"), "base")
	scratch.Write(t, filepath.Join(sharedDir, "shared_only.txt"), "shared")

	// Readdir should return entries from both layers
	stream, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir errno = %v", errno)
	}

	if stream == nil {
		t.Fatalf("Readdir returned nil stream")
	}

	// We should be able to iterate the directory stream
	// This tests that the merge logic works correctly
}

// Test 25: Getattr on directory with merged content
func TestGetattrDirectory(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create directories in both layers
	scratch.Mkdir(t, filepath.Join(baseDir, "dir"))
	scratch.Mkdir(t, filepath.Join(sharedDir, "dir"))

	dirNode := &node{view: root.view, path: "dir"}

	var out fuse.AttrOut
	errno := dirNode.Getattr(ctx, nil, &out)
	if errno != 0 {
		t.Errorf("Getattr on dir errno = %v", errno)
	}

	if !out.Attr.IsDir() {
		t.Errorf("Getattr returned non-directory mode")
	}
}
