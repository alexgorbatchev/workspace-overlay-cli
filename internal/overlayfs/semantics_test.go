package overlayfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"workspace-overlay/internal/scratch"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestMountReportsFilesystemStatistics(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}

	// Mount the project without any overlays
	_ = mountedProject(t, project)

	// Wait for the mount to be ready

	var mounted, backing syscall.Statfs_t
	if err := syscall.Statfs(project, &mounted); err != nil {
		t.Fatalf("Statfs on mounted: %v", err)
	}
	if err := syscall.Statfs(root, &backing); err != nil {
		t.Fatalf("Statfs on backing: %v", err)
	}

	if mounted.Blocks == 0 {
		t.Errorf("mounted.Blocks = 0, want non-zero")
	}
	if mounted.Blocks != backing.Blocks {
		t.Errorf("mounted.Blocks = %d, want %d", mounted.Blocks, backing.Blocks)
	}
	if mounted.Bsize != backing.Bsize {
		t.Errorf("mounted.Bsize = %d, want %d", mounted.Bsize, backing.Bsize)
	}
}

func TestMetadataChangeOnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test requires non-root user")
	}

	root, base, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create a file and make it unreadable (mode 0)
	lockedPath := filepath.Join(base, "locked.txt")
	scratch.Write(t, lockedPath, "x")
	if err := os.Chmod(lockedPath, 0); err != nil {
		t.Fatal(err)
	}

	// Create a node for the locked file
	child := &node{view: root.view, path: "locked.txt"}

	// Try to change the mode to 0644
	var in fuse.SetAttrIn
	in.Mode = 0644
	in.Valid |= fuse.FATTR_MODE

	var out fuse.AttrOut
	errno := child.Setattr(ctx, nil, &in, &out)
	if errno != 0 {
		t.Fatalf("Setattr chmod errno = %v (want 0)", errno)
	}

	// Verify the permissions were actually changed
	info, err := os.Stat(lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0644 {
		t.Errorf("file permission = %o, want 0644", perm)
	}
}

func TestStatfsOnNodeReturnsBackingStats(t *testing.T) {
	root, _, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Call Statfs directly on the node
	var out fuse.StatfsOut
	errno := root.Statfs(ctx, &out)
	if errno != 0 {
		t.Errorf("Statfs errno = %v (want 0)", errno)
	}
	if out.Blocks == 0 {
		t.Errorf("Statfs returned 0 blocks")
	}
	if out.Bsize == 0 {
		t.Errorf("Statfs returned 0 Bsize")
	}
}

func TestSetattrSizeChangeTakesOpenPath(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "truncate.txt"), "this is original content")
	child := &node{view: root.view, path: "truncate.txt"}

	// Setattr with size change should go through open path
	var in fuse.SetAttrIn
	in.Size = 5
	in.Valid |= fuse.FATTR_SIZE

	var out fuse.AttrOut
	errno := child.Setattr(ctx, nil, &in, &out)
	if errno != 0 {
		t.Fatalf("Setattr size errno = %v", errno)
	}

	// Verify truncation worked
	info, err := os.Stat(filepath.Join(baseDir, "truncate.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 5 {
		t.Errorf("file size = %d, want 5", info.Size())
	}
}

func TestReaddirEnumeratesAllLayers(t *testing.T) {
	root, base, shared, spec := setupTestRootNode(t)
	ctx := context.Background()

	// Create files in each layer
	scratch.Write(t, filepath.Join(base, "from_base.txt"), "base")
	scratch.Write(t, filepath.Join(shared, "from_shared.txt"), "shared")
	scratch.Write(t, filepath.Join(spec, "from_spec.txt"), "spec")

	// All files should be listed
	stream, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir errno = %v (want 0)", errno)
	}

	names := make(map[string]bool)
	for stream.HasNext() {
		entry, errno := stream.Next()
		if errno != 0 {
			t.Fatalf("stream.Next errno = %v", errno)
		}
		names[entry.Name] = true
	}

	if !names["from_base.txt"] {
		t.Errorf("from_base.txt not listed")
	}
	if !names["from_shared.txt"] {
		t.Errorf("from_shared.txt not listed")
	}
	if !names["from_spec.txt"] {
		t.Errorf("from_spec.txt not listed")
	}
}

func TestDirectoryWithCollisionStaysListable(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	ctx := context.Background()

	// Create base/sub directory with a file
	if err := os.MkdirAll(filepath.Join(base, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	scratch.Write(t, filepath.Join(base, "sub", "ordinary.txt"), "ordinary")

	// Create the collision: base has sub/conflict as a directory
	if err := os.MkdirAll(filepath.Join(base, "sub", "conflict"), 0755); err != nil {
		t.Fatal(err)
	}

	// Shared has sub as a directory (same as base)
	if err := os.MkdirAll(filepath.Join(shared, "sub"), 0755); err != nil {
		t.Fatal(err)
	}

	// Shared has sub/conflict as a FILE (type collision with base's directory)
	scratch.Write(t, filepath.Join(shared, "sub", "conflict"), "a file where the project has a directory")

	// Create a node for the directory
	dir := &node{view: root.view, path: "sub"}
	fs.NewNodeFS(dir, nil)

	// List the directory - should succeed even with collision
	stream, errno := dir.Readdir(ctx)
	if errno != 0 {
		t.Fatalf("Readdir errno = %v (want 0)", errno)
	}

	modes := make(map[string]uint32)
	for stream.HasNext() {
		entry, errno := stream.Next()
		if errno != 0 {
			t.Fatalf("stream.Next errno = %v", errno)
		}
		modes[entry.Name] = entry.Mode
	}
	if modes["ordinary.txt"]&syscall.S_IFMT != syscall.S_IFREG {
		t.Errorf("ordinary.txt listed with mode %o, want a regular file", modes["ordinary.txt"])
	}
	// The colliding entry must still carry a Unix file type, not a Go FileMode.
	if kind := modes["conflict"] & syscall.S_IFMT; kind != syscall.S_IFDIR && kind != syscall.S_IFREG {
		t.Errorf("conflict listed with mode %o, want a directory or regular file type", modes["conflict"])
	}

	// However, looking up "conflict" directly should fail with EIO due to type collision
	var entryOut fuse.EntryOut
	_, errno = dir.Lookup(ctx, "conflict", &entryOut)
	if errno != syscall.EIO {
		t.Errorf("Lookup conflict errno = %v, want EIO", errno)
	}

	// Verify the error is indeed a type collision
	_, err := root.view.resolve(filepath.Join(dir.relativePath(), "conflict"))
	if err == nil || !errors.Is(err, syscall.EIO) {
		t.Errorf("resolve conflict expected EIO, got %v", err)
	}
}
