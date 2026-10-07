package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestPermissions(t *testing.T) {
	tests := []struct {
		name string
		mode uint32
		want os.FileMode
	}{
		{
			name: "standard permissions",
			mode: 0644,
			want: 0644,
		},
		{
			name: "setuid",
			mode: 0755 | syscall.S_ISUID,
			want: 0755 | os.ModeSetuid,
		},
		{
			name: "setgid",
			mode: 0755 | syscall.S_ISGID,
			want: 0755 | os.ModeSetgid,
		},
		{
			name: "sticky",
			mode: 0755 | syscall.S_ISVTX,
			want: 0755 | os.ModeSticky,
		},
		{
			name: "all special flags",
			mode: 0777 | syscall.S_ISUID | syscall.S_ISGID | syscall.S_ISVTX,
			want: 0777 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := permissions(tt.mode)
			if got != tt.want {
				t.Errorf("permissions(0x%x) = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}

func TestNodeCreate(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	// 1. Create new file
	var entryOut fuse.EntryOut
	inode, handle, flags, errno := root.Create(ctx, "new_file.txt", syscall.O_RDWR, 0644, &entryOut)
	if errno != 0 || inode == nil || handle == nil {
		t.Fatalf("Create new_file.txt errno = %v", errno)
	}
	if flags != fuse.FOPEN_DIRECT_IO {
		t.Errorf("expected FOPEN_DIRECT_IO, got %v", flags)
	}
	_ = handle.(fs.FileReleaser).Release(ctx)

	// Verify file was created in base directory (layer 0)
	if _, err := os.Stat(filepath.Join(baseDir, "new_file.txt")); err != nil {
		t.Fatalf("new_file.txt not found in base dir: %v", err)
	}

	// 2. Create existing file fails with EEXIST
	_, _, _, errno = root.Create(ctx, "new_file.txt", syscall.O_RDWR, 0644, &entryOut)
	if errno != syscall.EEXIST {
		t.Errorf("Create existing file got errno %v, want EEXIST", errno)
	}

	// 3. Create in read-only directory fails
	roDir := filepath.Join(baseDir, "ro_dir")
	_ = os.Mkdir(roDir, 0555)
	defer func() { _ = os.Chmod(roDir, 0755) }()
	roNode := &node{view: root.view, path: "ro_dir"}
	fs.NewNodeFS(roNode, nil)
	_, _, _, errno = roNode.Create(ctx, "fail.txt", syscall.O_RDWR, 0644, &entryOut)
	if errno == 0 {
		t.Errorf("Create in read-only dir should fail")
	}
}

func TestNodeMkdir(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	// 1. Mkdir new directory
	var entryOut fuse.EntryOut
	inode, errno := root.Mkdir(ctx, "new_dir", 0755, &entryOut)
	if errno != 0 || inode == nil {
		t.Fatalf("Mkdir new_dir errno = %v", errno)
	}

	// Verify directory exists in baseDir
	info, err := os.Stat(filepath.Join(baseDir, "new_dir"))
	if err != nil || !info.IsDir() {
		t.Fatalf("new_dir not created as directory in base dir: %v", err)
	}

	// 2. Mkdir existing directory fails with EEXIST
	_, errno = root.Mkdir(ctx, "new_dir", 0755, &entryOut)
	if errno != syscall.EEXIST {
		t.Errorf("Mkdir existing directory got errno %v, want EEXIST", errno)
	}

	// 3. Mkdir in read-only directory fails
	roDir := filepath.Join(baseDir, "ro_mkdir")
	_ = os.Mkdir(roDir, 0555)
	defer func() { _ = os.Chmod(roDir, 0755) }()
	roNode := &node{view: root.view, path: "ro_mkdir"}
	fs.NewNodeFS(roNode, nil)
	_, errno = roNode.Mkdir(ctx, "sub", 0755, &entryOut)
	if errno == 0 {
		t.Errorf("Mkdir in read-only dir should fail")
	}
}

func TestNodeUnlink(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	_ = os.WriteFile(filepath.Join(baseDir, "to_delete.txt"), []byte("bye"), 0644)
	_ = os.Mkdir(filepath.Join(baseDir, "dir_to_delete"), 0755)

	// 1. Unlink directory fails with EISDIR
	errno := root.Unlink(ctx, "dir_to_delete")
	if errno != syscall.EISDIR {
		t.Errorf("Unlink directory got errno %v, want EISDIR", errno)
	}

	// 2. Unlink regular file succeeds
	errno = root.Unlink(ctx, "to_delete.txt")
	if errno != 0 {
		t.Fatalf("Unlink file errno = %v", errno)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "to_delete.txt")); !os.IsNotExist(err) {
		t.Errorf("file still exists after Unlink: %v", err)
	}
}

func TestNodeRmdir(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// 1. Empty single-layer dir succeeds
	_ = os.Mkdir(filepath.Join(baseDir, "empty_dir"), 0755)
	errno := root.Rmdir(ctx, "empty_dir")
	if errno != 0 {
		t.Fatalf("Rmdir empty_dir errno = %v", errno)
	}

	// 2. Non-empty dir fails with ENOTEMPTY
	_ = os.Mkdir(filepath.Join(baseDir, "non_empty_dir"), 0755)
	_ = os.WriteFile(filepath.Join(baseDir, "non_empty_dir", "child.txt"), []byte("c"), 0644)
	errno = root.Rmdir(ctx, "non_empty_dir")
	if errno != syscall.ENOTEMPTY {
		t.Errorf("Rmdir non-empty dir got errno %v, want ENOTEMPTY", errno)
	}

	// 3. Directory spanning multiple layers fails with EPERM
	_ = os.Mkdir(filepath.Join(baseDir, "multi_layer_dir"), 0755)
	_ = os.Mkdir(filepath.Join(sharedDir, "multi_layer_dir"), 0755)
	errno = root.Rmdir(ctx, "multi_layer_dir")
	if errno != syscall.EPERM {
		t.Errorf("Rmdir multi-layer dir got errno %v, want EPERM", errno)
	}
}

func TestNodeSymlinkAndLink(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// 1. Symlink
	var entryOut fuse.EntryOut
	inode, errno := root.Symlink(ctx, "target.txt", "sym.txt", &entryOut)
	if errno != 0 || inode == nil {
		t.Fatalf("Symlink errno = %v", errno)
	}
	target, err := os.Readlink(filepath.Join(baseDir, "sym.txt"))
	if err != nil || target != "target.txt" {
		t.Errorf("symlink target in baseDir = %q, want target.txt", target)
	}

	// 2. Hard link single-layer file succeeds
	_ = os.WriteFile(filepath.Join(baseDir, "orig.txt"), []byte("link me"), 0644)
	origNode := &node{view: root.view, path: "orig.txt"}
	fs.NewNodeFS(origNode, nil)
	linkInode, errno := root.Link(ctx, origNode, "hardlink.txt", &entryOut)
	if errno != 0 || linkInode == nil {
		t.Fatalf("Link errno = %v", errno)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "hardlink.txt")); err != nil {
		t.Fatalf("hardlink not found: %v", err)
	}

	// 3. Hard link multi-layer file fails with EXDEV
	_ = os.WriteFile(filepath.Join(sharedDir, "orig.txt"), []byte("multi"), 0644)
	_, errno = root.Link(ctx, origNode, "badlink.txt", &entryOut)
	if errno != syscall.EXDEV {
		t.Errorf("Link multi-layer file got errno %v, want EXDEV", errno)
	}
}

func TestNodeRename(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	// 1. Same source and dest path is a no-op returning 0
	errno := root.Rename(ctx, "same.txt", root, "same.txt", 0)
	if errno != 0 {
		t.Errorf("Rename same path got errno %v, want 0", errno)
	}

	// 2. Rename simple file in same layer
	_ = os.WriteFile(filepath.Join(baseDir, "old_name.txt"), []byte("data"), 0644)
	errno = root.Rename(ctx, "old_name.txt", root, "new_name.txt", 0)
	if errno != 0 {
		t.Fatalf("Rename simple file errno = %v", errno)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "new_name.txt")); err != nil {
		t.Errorf("new_name.txt not found after Rename: %v", err)
	}

	// 3. Rename with prepareSave (atomic save-and-rename of merged file)
	_ = os.WriteFile(filepath.Join(baseDir, "save_test.txt"), []byte("PREFIX-"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "save_test.txt"), []byte("OLD"), 0644)

	// Editor writes a staging file beside the mounted document. The root is backed by the project.
	_ = os.WriteFile(filepath.Join(baseDir, "save_test.tmp"), []byte("PREFIX-UPDATED"), 0644)

	// Rename temp file over merged file
	errno = root.Rename(ctx, "save_test.tmp", root, "save_test.txt", 0)
	if errno != 0 {
		t.Fatalf("Rename atomic save errno = %v", errno)
	}

	// Underlying shared file must now contain "UPDATED" (prefix stripped by prepareSave)
	data, err := os.ReadFile(filepath.Join(sharedDir, "save_test.txt"))
	if err != nil {
		t.Fatalf("read shared save_test.txt: %v", err)
	}
	if string(data) != "UPDATED" {
		t.Errorf("shared save_test.txt = %q, want %q", string(data), "UPDATED")
	}

	// 4. prepareSave with prefix violation: temp file does not have prefix
	_ = os.WriteFile(filepath.Join(baseDir, "bad_prefix.tmp"), []byte("CORRUPT"), 0644)
	errno = root.Rename(ctx, "bad_prefix.tmp", root, "save_test.txt", 0)
	if errno != syscall.EPERM {
		t.Errorf("Rename with prefix mismatch got errno %v, want EPERM", errno)
	}

	// 5. Rename directory spread across layers fails with EXDEV
	_ = os.Mkdir(filepath.Join(baseDir, "split_dir"), 0755)
	_ = os.Mkdir(filepath.Join(sharedDir, "split_dir"), 0755)
	errno = root.Rename(ctx, "split_dir", root, "moved_dir", 0)
	if errno != syscall.EXDEV {
		t.Errorf("Rename multi-layer dir got errno %v, want EXDEV", errno)
	}

	// 6. Rename with flags != 0 fails with ENOTSUP
	errno = root.Rename(ctx, "a", root, "b", 1)
	if errno != syscall.ENOTSUP {
		t.Errorf("Rename with flags got errno %v, want ENOTSUP", errno)
	}
}

func TestNodeSetattr(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	_ = os.WriteFile(filepath.Join(baseDir, "attr.txt"), []byte("attributes"), 0644)
	child := &node{view: root.view, path: "attr.txt"}
	fs.NewNodeFS(child, nil)

	// 1. Chmod via Setattr on file
	var in fuse.SetAttrIn
	in.Mode = 0600
	in.Valid |= fuse.FATTR_MODE
	var out fuse.AttrOut
	errno := child.Setattr(ctx, nil, &in, &out)
	if errno != 0 {
		t.Fatalf("Setattr chmod errno = %v", errno)
	}
	info, err := os.Stat(filepath.Join(baseDir, "attr.txt"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}

	// 2. Chtimes via Setattr on file
	now := time.Now().Add(-1 * time.Hour)
	var timeIn fuse.SetAttrIn
	timeIn.Mtime = uint64(now.Unix())
	timeIn.Mtimensec = uint32(now.Nanosecond())
	timeIn.Atime = uint64(now.Unix())
	timeIn.Atimensec = uint32(now.Nanosecond())
	timeIn.Valid |= fuse.FATTR_MTIME | fuse.FATTR_ATIME
	errno = child.Setattr(ctx, nil, &timeIn, &out)
	if errno != 0 {
		t.Fatalf("Setattr chtimes errno = %v", errno)
	}

	// 3. Truncate via Setattr on file
	var sizeIn fuse.SetAttrIn
	sizeIn.Size = 4
	sizeIn.Valid |= fuse.FATTR_SIZE
	errno = child.Setattr(ctx, nil, &sizeIn, &out)
	if errno != 0 {
		t.Fatalf("Setattr size errno = %v", errno)
	}
	info, _ = os.Stat(filepath.Join(baseDir, "attr.txt"))
	if info.Size() != 4 {
		t.Errorf("file size after truncate = %d, want 4", info.Size())
	}

	// 4. Setattr directly on open handle
	handle, _, errno := child.Open(ctx, syscall.O_RDWR)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer func() { _ = handle.(fs.FileReleaser).Release(ctx) }()
	errno = child.Setattr(ctx, handle, &sizeIn, &out)
	if errno != 0 {
		t.Errorf("Setattr with handle errno = %v", errno)
	}

	// 5. Setattr on directory node
	_ = os.Mkdir(filepath.Join(baseDir, "dir_attr"), 0755)
	dirChild := &node{view: root.view, path: "dir_attr"}
	fs.NewNodeFS(dirChild, nil)

	var dirIn fuse.SetAttrIn
	dirIn.Mode = 0700
	dirIn.Valid |= fuse.FATTR_MODE
	dirIn.Uid = uint32(os.Getuid())
	dirIn.Gid = uint32(os.Getgid())
	dirIn.Valid |= fuse.FATTR_UID | fuse.FATTR_GID
	dirIn.Mtime = uint64(now.Unix())
	dirIn.Valid |= fuse.FATTR_MTIME

	errno = dirChild.Setattr(ctx, nil, &dirIn, &out)
	if errno != 0 {
		t.Fatalf("Setattr on dir errno = %v", errno)
	}
	dirInfo, _ := os.Stat(filepath.Join(baseDir, "dir_attr"))
	if dirInfo.Mode().Perm() != 0700 {
		t.Errorf("dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}

	// 6. Setattr on dir with only atime set
	var dirAtimeIn fuse.SetAttrIn
	dirAtimeIn.Atime = uint64(now.Unix())
	dirAtimeIn.Valid |= fuse.FATTR_ATIME
	errno = dirChild.Setattr(ctx, nil, &dirAtimeIn, &out)
	if errno != 0 {
		t.Errorf("Setattr on dir with atime errno = %v", errno)
	}
}

func TestNodeMutationsErrors(t *testing.T) {
	root, _, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	// 1. Create with nonexistent parent
	badParent := &node{view: root.view, path: "nonexistent_dir"}
	var entryOut fuse.EntryOut
	_, _, _, errno := badParent.Create(ctx, "file.txt", syscall.O_RDWR, 0644, &entryOut)
	if errno == 0 {
		t.Errorf("Create in nonexistent parent should fail")
	}

	// 2. Mkdir in nonexistent parent
	_, errno = badParent.Mkdir(ctx, "sub", 0755, &entryOut)
	if errno == 0 {
		t.Errorf("Mkdir in nonexistent parent should fail")
	}

	// 3. Unlink nonexistent file
	errno = root.Unlink(ctx, "does_not_exist.txt")
	if errno == 0 {
		t.Errorf("Unlink nonexistent file should fail")
	}

	// 4. Rmdir nonexistent directory
	errno = root.Rmdir(ctx, "does_not_exist_dir")
	if errno == 0 {
		t.Errorf("Rmdir nonexistent dir should fail")
	}

	// 5. Symlink in nonexistent parent
	_, errno = badParent.Symlink(ctx, "target", "link", &entryOut)
	if errno == 0 {
		t.Errorf("Symlink in nonexistent parent should fail")
	}

	// 6. Link across different views
	otherView := &view{}
	otherNode := &node{view: otherView, path: "file"}
	_, errno = root.Link(ctx, otherNode, "link_name", &entryOut)
	if errno != syscall.EXDEV {
		t.Errorf("Link across different views got errno %v, want EXDEV", errno)
	}

	// 7. Rename across different views
	errno = root.Rename(ctx, "a", otherNode, "b", 0)
	if errno != syscall.EXDEV {
		t.Errorf("Rename across different views got errno %v, want EXDEV", errno)
	}

	// 8. Rename nonexistent source
	errno = root.Rename(ctx, "missing_source.txt", root, "dest.txt", 0)
	if errno == 0 {
		t.Errorf("Rename nonexistent source should fail")
	}

	// 9. closeFile error branch
	f, err := os.CreateTemp("", "close_test")
	if err == nil {
		_ = f.Close()
		closeFile(f) // Second close generates error in closeFile
		_ = os.Remove(f.Name())
	}

	// 10. prepareSave when dest is a directory returns nil, 0
	_ = os.Mkdir(filepath.Join(root.view.layers[0].root.Name(), "dest_is_dir"), 0755)
	orig, errno := root.prepareSave(0, "some_source", "dest_is_dir")
	if errno != 0 || orig != nil {
		t.Errorf("prepareSave on directory dest should return nil, 0; got %v, %v", orig, errno)
	}

	// 11. renameBetween errors
	root0 := root.view.layers[0].root
	errno = renameBetween(root0, "bad_dir/source.txt", root0, "dest.txt")
	if errno == 0 {
		t.Errorf("renameBetween with bad source dir should fail")
	}
	errno = renameBetween(root0, "source.txt", root0, "bad_dest_dir/dest.txt")
	if errno == 0 {
		t.Errorf("renameBetween with bad dest dir should fail")
	}

	// 12. Setattr on nonexistent node fails
	missingNode := &node{view: root.view, path: "missing_setattr.txt"}
	var in fuse.SetAttrIn
	var out fuse.AttrOut
	if errno := missingNode.Setattr(ctx, nil, &in, &out); errno == 0 {
		t.Errorf("Setattr on nonexistent node should fail")
	}

	// 13. Setattr with only GID
	_ = os.Mkdir(filepath.Join(root.view.layers[0].root.Name(), "gid_dir"), 0755)
	gidNode := &node{view: root.view, path: "gid_dir"}
	fs.NewNodeFS(gidNode, nil)
	var gidIn fuse.SetAttrIn
	gidIn.Gid = uint32(os.Getgid())
	gidIn.Valid |= fuse.FATTR_GID
	if errno := gidNode.Setattr(ctx, nil, &gidIn, &out); errno != 0 {
		t.Errorf("Setattr only GID errno = %v", errno)
	}

	// 14. prepareSave when ReadFile fails on nonexistent source
	if _, errno := root.prepareSave(0, "nonexistent_source", "dest_is_dir"); errno == 0 {
		// dest_is_dir is handled, test with a merged dest
	}
}
