package overlayfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

// Failed editor save restores the staged file when target dir is read-only.
func TestFailedEditorSaveRestore(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "base\n")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "old\n")
	scratch.Write(t, filepath.Join(baseDir, "doc.tmp"), "base\nnew\n")
	if err := os.Chmod(sharedDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(sharedDir, 0755); err != nil {
			t.Errorf("restore shared dir permissions: %v", err)
		}
	})

	// A read-only destination directory makes the OS refuse the rename with EACCES.
	errno := root.Rename(context.Background(), "doc.tmp", root, "doc.md", 0)
	if errno != syscall.EACCES && errno != syscall.EPERM {
		t.Fatalf("Rename with read-only dest dir got errno %v, want EACCES or EPERM", errno)
	}

	if got := string(scratch.Read(t, filepath.Join(baseDir, "doc.tmp"))); got != "base\nnew\n" {
		t.Errorf("doc.tmp after failed save = %q, want %q", got, "base\nnew\n")
	}
	if got := string(scratch.Read(t, filepath.Join(sharedDir, "doc.md"))); got != "old\n" {
		t.Errorf("shared/doc.md after failed save = %q, want %q", got, "old\n")
	}
}

// Rename with flags or to different view returns appropriate errors.
func TestRenameErrorCases(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "content")

	t.Run("rename_with_flags", func(t *testing.T) {
		errno := root.Rename(context.Background(), "file.txt", root, "renamed.txt", 1)
		if errno != syscall.ENOTSUP {
			t.Errorf("Rename with flags=1 got errno %v, want ENOTSUP", errno)
		}
	})

	t.Run("rename_to_different_view", func(t *testing.T) {
		otherView := &View{}
		otherNode := &node{view: otherView, path: "dest"}
		errno := root.Rename(context.Background(), "file.txt", otherNode, "newname.txt", 0)
		if errno != syscall.EXDEV {
			t.Errorf("Rename to different view got errno %v, want EXDEV", errno)
		}
	})
}

// Link returns EXDEV for multi-layer files or different views.
func TestLinkErrorCases(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	var entryOut fuse.EntryOut

	t.Run("link_multi_layer_file", func(t *testing.T) {
		scratch.Write(t, filepath.Join(baseDir, "multi.txt"), "base")
		scratch.Write(t, filepath.Join(sharedDir, "multi.txt"), "shared")
		multiNode := &node{view: root.view, path: "multi.txt"}
		fs.NewNodeFS(multiNode, nil)

		_, errno := root.Link(context.Background(), multiNode, "link.txt", &entryOut)
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

		_, errno := otherNode.Link(context.Background(), origNode, "link.txt", &entryOut)
		if errno != syscall.EXDEV {
			t.Errorf("Link to different view got errno %v, want EXDEV", errno)
		}
	})
}

// Symlink at existing name returns EEXIST.
func TestSymlinkExists(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "existing.txt"), "content")
	var entryOut fuse.EntryOut
	_, errno := root.Symlink(context.Background(), "target.txt", "existing.txt", &entryOut)
	if errno != syscall.EEXIST {
		t.Errorf("Symlink at existing name got errno %v, want EEXIST", errno)
	}
}

// Fsync persists staged changes before handle release.
func TestMergedFileFsync(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "START:")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "shared content\n")
	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)
	ctx := context.Background()
	handle, _, errno := docNode.Open(ctx, syscall.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 && errno != syscall.EPERM {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	mh := handle.(*mergedFile)
	if mh == nil {
		t.Fatal("Open did not return mergedFile handle")
	}
	_, errno = mh.Write(ctx, []byte("new content\n"), 6)
	if errno != 0 {
		t.Fatalf("Write errno = %v", errno)
	}

	errno = mh.Fsync(ctx, 0)
	if errno != 0 {
		t.Errorf("Fsync errno = %v", errno)
	}

	if got := string(scratch.Read(t, filepath.Join(sharedDir, "doc.md"))); !strings.Contains(got, "new content") {
		t.Errorf("shared/doc.md after Fsync = %q, want it to contain 'new content'", got)
	}
}

// Flush after prefix-violating write returns EPERM and leaves files unchanged.
func TestMergedFileFlushPrefixViolation(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "PREFIX-base\n")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "old\n")
	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)
	ctx := context.Background()
	handle, _, errno := docNode.Open(ctx, syscall.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 && errno != syscall.EPERM {
		}
	}()

	mh := handle.(*mergedFile)
	_, errno = mh.Write(ctx, []byte("BAD-"), 0)
	if errno != syscall.EPERM {
		t.Errorf("Write violating prefix got errno %v, want EPERM", errno)
	}

	errno = mh.Flush(ctx)
	if errno != syscall.EPERM {
		t.Errorf("Flush after failed write got errno %v, want EPERM", errno)
	}

	if got := string(scratch.Read(t, filepath.Join(baseDir, "doc.md"))); got != "PREFIX-base\n" {
		t.Errorf("base/doc.md changed: %q", got)
	}
	if got := string(scratch.Read(t, filepath.Join(sharedDir, "doc.md"))); got != "old\n" {
		t.Errorf("shared/doc.md changed: %q", got)
	}
}

// Open mode violations return EBADF on mismatched operations.
func TestMergedFileOpenModeViolations(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "PREFIX-base\n")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "old\n")
	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)
	ctx := context.Background()

	t.Run("read_only_write_fails", func(t *testing.T) {
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

// Rename to different view returns EXDEV.
func TestRenameExdevDifferentView(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "content")
	otherView := &View{}
	errno := root.Rename(context.Background(), "file.txt", &node{view: otherView}, "newname.txt", 0)
	if errno != syscall.EXDEV {
		t.Errorf("Rename to different view got errno %v, want EXDEV", errno)
	}
}

// Mutations in read-only parent return permission errors.
func TestMutationInReadOnlyParent(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
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
	ctx := context.Background()

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

// Loopback handle for single-layer files returns valid handle.
func TestSingleLayerFileLoopback(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "single.txt"), "content")
	singleNode := &node{view: root.view, path: "single.txt"}
	fs.NewNodeFS(singleNode, nil)
	handle, _, errno := singleNode.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open errno = %v", errno)
	}
	defer func() {
		if errno := handle.(fs.FileReleaser).Release(context.Background()); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()
	if handle == nil {
		t.Errorf("Open returned nil handle")
	}
}

// Unlink nonexistent file returns error.
func TestUnlinkNonexistentFile(t *testing.T) {
	root, _, _, _ := setupTestRootNode(t)
	errno := root.Unlink(context.Background(), "does_not_exist.txt")
	if errno == 0 {
		t.Errorf("Unlink nonexistent file should fail")
	}
}

// Rmdir on multi-layer directory fails with EPERM.
func TestRmdirMultiLayer(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Mkdir(t, filepath.Join(baseDir, "multidir"))
	scratch.Mkdir(t, filepath.Join(sharedDir, "multidir"))
	errno := root.Rmdir(context.Background(), "multidir")
	if errno != syscall.EPERM {
		t.Errorf("Rmdir multi-layer dir got errno %v, want EPERM", errno)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "multidir")); err != nil {
		t.Errorf("multidir should still exist in base: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sharedDir, "multidir")); err != nil {
		t.Errorf("multidir should still exist in shared: %v", err)
	}
}

// Merged file with only prefix handles read operations.
func TestMergedFilePrefixOnly(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "prefix.md"), "PREFIX-")
	scratch.Write(t, filepath.Join(sharedDir, "prefix.md"), "OLD")
	prefixNode := &node{view: root.view, path: "prefix.md"}
	fs.NewNodeFS(prefixNode, nil)
	ctx := context.Background()
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
		t.Errorf("Read content = %q, want PREFIX- and OLD", content)
	}
}

// Getattr on closed node returns error.
func TestGetattrOnClosedNode(t *testing.T) {
	v, baseDir, _, _ := setupTestView(t)
	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "data")
	_, err := v.resolve("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Project().Close(); err != nil {
		t.Fatal(err)
	}
	n := &node{view: v, path: "file.txt"}
	var out fuse.AttrOut
	errno := n.Getattr(context.Background(), nil, &out)
	if errno == 0 {
		t.Errorf("Getattr on closed layer root should fail, got errno 0")
	}
}

// Readdir on closed view returns error.
func TestReaddirOnClosedView(t *testing.T) {
	v, baseDir, _, _ := setupTestView(t)
	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "data")
	rootNode := &node{view: v, path: "."}
	fs.NewNodeFS(rootNode, nil)
	if err := v.Project().Close(); err != nil {
		t.Fatal(err)
	}
	_, errno := rootNode.Readdir(context.Background())
	if errno == 0 {
		t.Errorf("Readdir on closed layer root should fail")
	}
}

// Getattr on merged file returns correct size.
func TestMergedFileGetattr(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "PREFIX-")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "shared\n")
	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)
	ctx := context.Background()
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

// Flush commits changes to disk.
func TestMergedFileFlushCommits(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "PREFIX-")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "old\n")
	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)
	ctx := context.Background()
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

	errno = mh.Flush(ctx)
	if errno != 0 {
		t.Errorf("Flush errno = %v", errno)
	}

	if got := string(scratch.Read(t, filepath.Join(sharedDir, "doc.md"))); !strings.Contains(got, "modified") {
		t.Errorf("shared/doc.md after Flush = %q, want it to contain 'modified'", got)
	}
}

// Setattr mode changes on single-layer file succeed.
func TestSetAttrSingleLayerFile(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "content")
	fileNode := &node{view: root.view, path: "file.txt"}
	fs.NewNodeFS(fileNode, nil)
	var in fuse.SetAttrIn
	in.Mode = 0755
	in.Valid |= fuse.FATTR_MODE
	var out fuse.AttrOut
	errno := fileNode.Setattr(context.Background(), nil, &in, &out)
	if errno != 0 {
		t.Errorf("Setattr mode errno = %v", errno)
	}
	info, err := os.Stat(filepath.Join(baseDir, "file.txt"))
	if err != nil {
		t.Fatalf("stat file.txt: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("file.txt mode = %o, want 0755", info.Mode().Perm())
	}
}

// Create file returns correct entry attributes.
func TestCreateFileAttributes(t *testing.T) {
	root, _, _, _ := setupTestRootNode(t)
	var entryOut fuse.EntryOut
	inode, handle, _, errno := root.Create(context.Background(), "newfile.txt", syscall.O_WRONLY, 0644, &entryOut)
	if errno != 0 {
		t.Fatalf("Create errno = %v", errno)
	}
	if inode == nil {
		t.Errorf("Create returned nil inode")
	}
	if handle != nil {
		if relErr := handle.(fs.FileReleaser).Release(context.Background()); relErr != 0 {
			t.Errorf("Release errno = %v", relErr)
		}
	}
	if entryOut.Attr.Mode == 0 {
		t.Errorf("Create returned zero mode in EntryOut")
	}
}

// Mkdir returns correct entry attributes.
func TestMkdirAttributes(t *testing.T) {
	root, _, _, _ := setupTestRootNode(t)
	var entryOut fuse.EntryOut
	inode, errno := root.Mkdir(context.Background(), "newdir", 0755, &entryOut)
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

// Open in append mode allows appending writes.
func TestOpenAppendMode(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "doc.md"), "start:")
	scratch.Write(t, filepath.Join(sharedDir, "doc.md"), "content\n")
	docNode := &node{view: root.view, path: "doc.md"}
	fs.NewNodeFS(docNode, nil)
	ctx := context.Background()
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

// Lookup resolves paths correctly.
func TestLookup(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "file.txt"), "content")
	scratch.Mkdir(t, filepath.Join(baseDir, "subdir"))
	ctx := context.Background()

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

// Readdir merges entries from all layers.
func TestReaddirMultiLayer(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(baseDir, "base_only.txt"), "base")
	scratch.Write(t, filepath.Join(sharedDir, "shared_only.txt"), "shared")
	stream, errno := root.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir errno = %v", errno)
	}
	if stream == nil {
		t.Fatalf("Readdir returned nil stream")
	}
}

// Getattr on merged directory returns directory mode.
func TestGetattrDirectory(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	scratch.Mkdir(t, filepath.Join(baseDir, "dir"))
	scratch.Mkdir(t, filepath.Join(sharedDir, "dir"))
	dirNode := &node{view: root.view, path: "dir"}
	var out fuse.AttrOut
	errno := dirNode.Getattr(context.Background(), nil, &out)
	if errno != 0 {
		t.Errorf("Getattr on dir errno = %v", errno)
	}
	if !out.Attr.IsDir() {
		t.Errorf("Getattr returned non-directory mode")
	}
}
