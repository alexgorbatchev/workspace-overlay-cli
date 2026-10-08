package overlayfs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
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

	var entryOut fuse.EntryOut

	t.Run("create_new_file", func(t *testing.T) {
		inode, handle, flags, errno := root.Create(ctx, "new_file.txt", syscall.O_RDWR, 0644, &entryOut)
		if errno != 0 || inode == nil || handle == nil {
			t.Fatalf("Create new_file.txt errno = %v", errno)
		}
		if flags != 0 {
			t.Errorf("expected flags 0 for newly created file, got %v", flags)
		}
		if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Fatalf("Release handle errno = %v", errno)
		}

		// Verify file was created in base directory (layer 0)
		if _, err := os.Stat(filepath.Join(baseDir, "new_file.txt")); err != nil {
			t.Fatalf("new_file.txt not found in base dir: %v", err)
		}
	})

	t.Run("create_existing_file_eexist", func(t *testing.T) {
		_, _, _, errno := root.Create(ctx, "new_file.txt", syscall.O_RDWR, 0644, &entryOut)
		if errno != syscall.EEXIST {
			t.Errorf("Create existing file got errno %v, want EEXIST", errno)
		}
	})

	t.Run("create_in_readonly_dir_fails", func(t *testing.T) {
		roDir := filepath.Join(baseDir, "ro_dir")
		if err := os.Mkdir(roDir, 0555); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Chmod(roDir, 0755); err != nil {
				t.Errorf("Chmod cleanup failed: %v", err)
			}
		}()
		roNode := &node{view: root.view, path: "ro_dir"}
		fs.NewNodeFS(roNode, nil)
		_, _, _, errno := roNode.Create(ctx, "fail.txt", syscall.O_RDWR, 0644, &entryOut)
		if errno == 0 {
			t.Errorf("Create in read-only dir should fail")
		}
	})
}

func TestNodeMkdir(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	var entryOut fuse.EntryOut

	t.Run("mkdir_new_directory", func(t *testing.T) {
		inode, errno := root.Mkdir(ctx, "new_dir", 0755, &entryOut)
		if errno != 0 || inode == nil {
			t.Fatalf("Mkdir new_dir errno = %v", errno)
		}

		// Verify directory exists in baseDir
		info, err := os.Stat(filepath.Join(baseDir, "new_dir"))
		if err != nil || !info.IsDir() {
			t.Fatalf("new_dir not created as directory in base dir: %v", err)
		}
	})

	t.Run("mkdir_existing_directory_eexist", func(t *testing.T) {
		_, errno := root.Mkdir(ctx, "new_dir", 0755, &entryOut)
		if errno != syscall.EEXIST {
			t.Errorf("Mkdir existing directory got errno %v, want EEXIST", errno)
		}
	})

	t.Run("mkdir_in_readonly_dir_fails", func(t *testing.T) {
		roDir := filepath.Join(baseDir, "ro_mkdir")
		if err := os.Mkdir(roDir, 0555); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Chmod(roDir, 0755); err != nil {
				t.Errorf("Chmod cleanup failed: %v", err)
			}
		}()
		roNode := &node{view: root.view, path: "ro_mkdir"}
		fs.NewNodeFS(roNode, nil)
		_, errno := roNode.Mkdir(ctx, "sub", 0755, &entryOut)
		if errno == 0 {
			t.Errorf("Mkdir in read-only dir should fail")
		}
	})
}

func TestNodeUnlink(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "to_delete.txt"), "bye")
	scratch.Mkdir(t, filepath.Join(baseDir, "dir_to_delete"))

	t.Run("unlink_directory_eisdir", func(t *testing.T) {
		errno := root.Unlink(ctx, "dir_to_delete")
		if errno != syscall.EISDIR {
			t.Errorf("Unlink directory got errno %v, want EISDIR", errno)
		}
	})

	t.Run("unlink_regular_file", func(t *testing.T) {
		errno := root.Unlink(ctx, "to_delete.txt")
		if errno != 0 {
			t.Fatalf("Unlink file errno = %v", errno)
		}
		if _, err := os.Stat(filepath.Join(baseDir, "to_delete.txt")); !os.IsNotExist(err) {
			t.Errorf("file still exists after Unlink: %v", err)
		}
	})
}

func TestNodeRmdir(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	t.Run("rmdir_empty_single_layer", func(t *testing.T) {
		scratch.Mkdir(t, filepath.Join(baseDir, "empty_dir"))
		errno := root.Rmdir(ctx, "empty_dir")
		if errno != 0 {
			t.Fatalf("Rmdir empty_dir errno = %v", errno)
		}
	})

	t.Run("rmdir_non_empty_enotempty", func(t *testing.T) {
		scratch.Mkdir(t, filepath.Join(baseDir, "non_empty_dir"))
		scratch.Write(t, filepath.Join(baseDir, "non_empty_dir", "child.txt"), "c")
		errno := root.Rmdir(ctx, "non_empty_dir")
		if errno != syscall.ENOTEMPTY {
			t.Errorf("Rmdir non-empty dir got errno %v, want ENOTEMPTY", errno)
		}
	})

	t.Run("rmdir_multi_layer_eperm", func(t *testing.T) {
		scratch.Mkdir(t, filepath.Join(baseDir, "multi_layer_dir"))
		scratch.Mkdir(t, filepath.Join(sharedDir, "multi_layer_dir"))
		errno := root.Rmdir(ctx, "multi_layer_dir")
		if errno != syscall.EPERM {
			t.Errorf("Rmdir multi-layer dir got errno %v, want EPERM", errno)
		}
	})
}

func TestNodeSymlinkAndLink(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	var entryOut fuse.EntryOut

	t.Run("symlink", func(t *testing.T) {
		inode, errno := root.Symlink(ctx, "target.txt", "sym.txt", &entryOut)
		if errno != 0 || inode == nil {
			t.Fatalf("Symlink errno = %v", errno)
		}
		target, err := os.Readlink(filepath.Join(baseDir, "sym.txt"))
		if err != nil || target != "target.txt" {
			t.Errorf("symlink target in baseDir = %q, want target.txt", target)
		}
	})

	t.Run("link_single_layer_file", func(t *testing.T) {
		scratch.Write(t, filepath.Join(baseDir, "orig.txt"), "link me")
		origNode := &node{view: root.view, path: "orig.txt"}
		fs.NewNodeFS(origNode, nil)
		linkInode, errno := root.Link(ctx, origNode, "hardlink.txt", &entryOut)
		if errno != 0 || linkInode == nil {
			t.Fatalf("Link errno = %v", errno)
		}
		if _, err := os.Stat(filepath.Join(baseDir, "hardlink.txt")); err != nil {
			t.Fatalf("hardlink not found: %v", err)
		}
	})

	t.Run("link_multi_layer_file_exdev", func(t *testing.T) {
		scratch.Write(t, filepath.Join(sharedDir, "orig.txt"), "multi")
		origNode := &node{view: root.view, path: "orig.txt"}
		fs.NewNodeFS(origNode, nil)
		_, errno := root.Link(ctx, origNode, "badlink.txt", &entryOut)
		if errno != syscall.EXDEV {
			t.Errorf("Link multi-layer file got errno %v, want EXDEV", errno)
		}
	})
}

func TestNodeRename(t *testing.T) {
	root, baseDir, sharedDir, _ := setupTestRootNode(t)
	ctx := context.Background()

	t.Run("rename_same_path_noop", func(t *testing.T) {
		errno := root.Rename(ctx, "same.txt", root, "same.txt", 0)
		if errno != 0 {
			t.Errorf("Rename same path got errno %v, want 0", errno)
		}
	})

	t.Run("rename_simple_file", func(t *testing.T) {
		scratch.Write(t, filepath.Join(baseDir, "old_name.txt"), "data")
		errno := root.Rename(ctx, "old_name.txt", root, "new_name.txt", 0)
		if errno != 0 {
			t.Fatalf("Rename simple file errno = %v", errno)
		}
		if _, err := os.Stat(filepath.Join(baseDir, "new_name.txt")); err != nil {
			t.Errorf("new_name.txt not found after Rename: %v", err)
		}
	})

	t.Run("rename_with_prepareSave", func(t *testing.T) {
		scratch.Write(t, filepath.Join(baseDir, "save_test.txt"), "PREFIX-")
		scratch.Write(t, filepath.Join(sharedDir, "save_test.txt"), "OLD")

		// Editor writes a staging file beside the mounted document. The root is backed by the project.
		scratch.Write(t, filepath.Join(baseDir, "save_test.tmp"), "PREFIX-UPDATED")

		// Rename temp file over merged file
		errno := root.Rename(ctx, "save_test.tmp", root, "save_test.txt", 0)
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
	})

	t.Run("rename_prefix_violation", func(t *testing.T) {
		scratch.Write(t, filepath.Join(baseDir, "bad_prefix.tmp"), "CORRUPT")
		errno := root.Rename(ctx, "bad_prefix.tmp", root, "save_test.txt", 0)
		if errno != syscall.EPERM {
			t.Errorf("Rename with prefix mismatch got errno %v, want EPERM", errno)
		}
	})

	t.Run("rename_multi_layer_dir_exdev", func(t *testing.T) {
		scratch.Mkdir(t, filepath.Join(baseDir, "split_dir"))
		scratch.Mkdir(t, filepath.Join(sharedDir, "split_dir"))
		errno := root.Rename(ctx, "split_dir", root, "moved_dir", 0)
		if errno != syscall.EXDEV {
			t.Errorf("Rename multi-layer dir got errno %v, want EXDEV", errno)
		}
	})

	t.Run("rename_with_flags_enotsup", func(t *testing.T) {
		errno := root.Rename(ctx, "a", root, "b", 1)
		if errno != syscall.ENOTSUP {
			t.Errorf("Rename with flags got errno %v, want ENOTSUP", errno)
		}
	})
}

func TestNodeSetattr(t *testing.T) {
	root, baseDir, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	scratch.Write(t, filepath.Join(baseDir, "attr.txt"), "attributes")
	child := &node{view: root.view, path: "attr.txt"}
	fs.NewNodeFS(child, nil)

	var out fuse.AttrOut

	t.Run("chmod", func(t *testing.T) {
		var in fuse.SetAttrIn
		in.Mode = 0600
		in.Valid |= fuse.FATTR_MODE
		errno := child.Setattr(ctx, nil, &in, &out)
		if errno != 0 {
			t.Fatalf("Setattr chmod errno = %v", errno)
		}
		info, err := os.Stat(filepath.Join(baseDir, "attr.txt"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
		}
	})

	t.Run("chtimes", func(t *testing.T) {
		now := time.Now().Add(-1 * time.Hour)
		var timeIn fuse.SetAttrIn
		timeIn.Mtime = uint64(now.Unix())
		timeIn.Mtimensec = uint32(now.Nanosecond())
		timeIn.Atime = uint64(now.Unix())
		timeIn.Atimensec = uint32(now.Nanosecond())
		timeIn.Valid |= fuse.FATTR_MTIME | fuse.FATTR_ATIME
		errno := child.Setattr(ctx, nil, &timeIn, &out)
		if errno != 0 {
			t.Fatalf("Setattr chtimes errno = %v", errno)
		}
	})

	t.Run("truncate", func(t *testing.T) {
		var sizeIn fuse.SetAttrIn
		sizeIn.Size = 4
		sizeIn.Valid |= fuse.FATTR_SIZE
		errno := child.Setattr(ctx, nil, &sizeIn, &out)
		if errno != 0 {
			t.Fatalf("Setattr size errno = %v", errno)
		}
		info, err := os.Stat(filepath.Join(baseDir, "attr.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 4 {
			t.Errorf("file size after truncate = %d, want 4", info.Size())
		}
	})

	t.Run("setattr_on_handle", func(t *testing.T) {
		var sizeIn fuse.SetAttrIn
		sizeIn.Size = 4
		sizeIn.Valid |= fuse.FATTR_SIZE
		handle, _, errno := child.Open(ctx, syscall.O_RDWR)
		if errno != 0 {
			t.Fatal(errno)
		}
		defer func() {
			if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
				t.Errorf("Release handle errno = %v", errno)
			}
		}()
		errno = child.Setattr(ctx, handle, &sizeIn, &out)
		if errno != 0 {
			t.Errorf("Setattr with handle errno = %v", errno)
		}
	})

	t.Run("setattr_on_directory", func(t *testing.T) {
		scratch.Mkdir(t, filepath.Join(baseDir, "dir_attr"))
		dirChild := &node{view: root.view, path: "dir_attr"}
		fs.NewNodeFS(dirChild, nil)

		now := time.Now().Add(-1 * time.Hour)
		var dirIn fuse.SetAttrIn
		dirIn.Mode = 0700
		dirIn.Valid |= fuse.FATTR_MODE
		dirIn.Uid = uint32(os.Getuid())
		dirIn.Gid = uint32(os.Getgid())
		dirIn.Valid |= fuse.FATTR_UID | fuse.FATTR_GID
		dirIn.Mtime = uint64(now.Unix())
		dirIn.Valid |= fuse.FATTR_MTIME

		errno := dirChild.Setattr(ctx, nil, &dirIn, &out)
		if errno != 0 {
			t.Fatalf("Setattr on dir errno = %v", errno)
		}
		dirInfo, err := os.Stat(filepath.Join(baseDir, "dir_attr"))
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != 0700 {
			t.Errorf("dir mode = %v, want 0700", dirInfo.Mode().Perm())
		}
	})

	t.Run("setattr_on_dir_atime", func(t *testing.T) {
		scratch.Mkdir(t, filepath.Join(baseDir, "dir_attr"))
		dirChild := &node{view: root.view, path: "dir_attr"}
		fs.NewNodeFS(dirChild, nil)

		now := time.Now().Add(-1 * time.Hour)
		var dirAtimeIn fuse.SetAttrIn
		dirAtimeIn.Atime = uint64(now.Unix())
		dirAtimeIn.Valid |= fuse.FATTR_ATIME
		errno := dirChild.Setattr(ctx, nil, &dirAtimeIn, &out)
		if errno != 0 {
			t.Errorf("Setattr on dir with atime errno = %v", errno)
		}
	})
}

func TestNodeMutationsErrors(t *testing.T) {
	root, _, _, _ := setupTestRootNode(t)
	ctx := context.Background()

	var entryOut fuse.EntryOut

	t.Run("create_nonexistent_parent", func(t *testing.T) {
		badParent := &node{view: root.view, path: "nonexistent_dir"}
		_, _, _, errno := badParent.Create(ctx, "file.txt", syscall.O_RDWR, 0644, &entryOut)
		if errno == 0 {
			t.Errorf("Create in nonexistent parent should fail")
		}
	})

	t.Run("mkdir_nonexistent_parent", func(t *testing.T) {
		badParent := &node{view: root.view, path: "nonexistent_dir"}
		_, errno := badParent.Mkdir(ctx, "sub", 0755, &entryOut)
		if errno == 0 {
			t.Errorf("Mkdir in nonexistent parent should fail")
		}
	})

	t.Run("unlink_nonexistent_file", func(t *testing.T) {
		errno := root.Unlink(ctx, "does_not_exist.txt")
		if errno == 0 {
			t.Errorf("Unlink nonexistent file should fail")
		}
	})

	t.Run("rmdir_nonexistent_dir", func(t *testing.T) {
		errno := root.Rmdir(ctx, "does_not_exist_dir")
		if errno == 0 {
			t.Errorf("Rmdir nonexistent dir should fail")
		}
	})

	t.Run("symlink_nonexistent_parent", func(t *testing.T) {
		badParent := &node{view: root.view, path: "nonexistent_dir"}
		_, errno := badParent.Symlink(ctx, "target", "link", &entryOut)
		if errno == 0 {
			t.Errorf("Symlink in nonexistent parent should fail")
		}
	})

	t.Run("link_different_views_exdev", func(t *testing.T) {
		otherView := &View{}
		otherNode := &node{view: otherView, path: "file"}
		_, errno := root.Link(ctx, otherNode, "link_name", &entryOut)
		if errno != syscall.EXDEV {
			t.Errorf("Link across different views got errno %v, want EXDEV", errno)
		}
	})

	t.Run("rename_different_views_exdev", func(t *testing.T) {
		otherView := &View{}
		otherNode := &node{view: otherView, path: "file"}
		errno := root.Rename(ctx, "a", otherNode, "b", 0)
		if errno != syscall.EXDEV {
			t.Errorf("Rename across different views got errno %v, want EXDEV", errno)
		}
	})

	t.Run("rename_nonexistent_source", func(t *testing.T) {
		errno := root.Rename(ctx, "missing_source.txt", root, "dest.txt", 0)
		if errno == 0 {
			t.Errorf("Rename nonexistent source should fail")
		}
	})

	t.Run("closeFile_error", func(t *testing.T) {
		f, err := os.CreateTemp("", "close_test")
		if err == nil {
			if err := f.Close(); err != nil {
				t.Errorf("first close error: %v", err)
			}
			logged.Close(f) // Second close generates error in closeFile
			if err := os.Remove(f.Name()); err != nil {
				t.Errorf("Remove error: %v", err)
			}
		}
	})

	t.Run("prepareSave_directory_dest", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(root.view.layers[0].root.Name(), "dest_is_dir"), 0755); err != nil {
			t.Fatal(err)
		}
		orig, errno := root.prepareSave(0, "some_source", "dest_is_dir")
		if errno != 0 || orig != nil {
			t.Errorf("prepareSave on directory dest should return nil, 0; got %v, %v", orig, errno)
		}
	})

	t.Run("renameBetween_errors", func(t *testing.T) {
		root0 := root.view.layers[0].root
		errno := renameBetween(root0, "bad_dir/source.txt", root0, "dest.txt")
		if errno == 0 {
			t.Errorf("renameBetween with bad source dir should fail")
		}
		errno = renameBetween(root0, "source.txt", root0, "bad_dest_dir/dest.txt")
		if errno == 0 {
			t.Errorf("renameBetween with bad dest dir should fail")
		}
	})

	t.Run("setattr_nonexistent_node", func(t *testing.T) {
		missingNode := &node{view: root.view, path: "missing_setattr.txt"}
		var in fuse.SetAttrIn
		var out fuse.AttrOut
		if errno := missingNode.Setattr(ctx, nil, &in, &out); errno == 0 {
			t.Errorf("Setattr on nonexistent node should fail")
		}
	})

	t.Run("setattr_only_gid", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(root.view.layers[0].root.Name(), "gid_dir"), 0755); err != nil {
			t.Fatal(err)
		}
		gidNode := &node{view: root.view, path: "gid_dir"}
		fs.NewNodeFS(gidNode, nil)
		var gidIn fuse.SetAttrIn
		gidIn.Gid = uint32(os.Getgid())
		gidIn.Valid |= fuse.FATTR_GID
		var out fuse.AttrOut
		if errno := gidNode.Setattr(ctx, nil, &gidIn, &out); errno != 0 {
			t.Errorf("Setattr only GID errno = %v", errno)
		}
	})

	t.Run("prepareSave_nonexistent_source", func(t *testing.T) {
		// The staged file is only read when the destination is a merged file.
		for _, layer := range root.view.layers[:2] {
			scratch.Write(t, filepath.Join(layer.root.Name(), "merged_save.txt"), "content\n")
		}
		if _, errno := root.prepareSave(0, "nonexistent_source", "merged_save.txt"); errno != syscall.ENOENT {
			t.Errorf("prepareSave with a missing staged file = %v, want ENOENT", errno)
		}
	})
}

func TestUnlinkCollidingFileRemovesProjectBacking(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	ctx := context.Background()
	scratch.Write(t, filepath.Join(base, "agents.md"), "project rules")
	scratch.Write(t, filepath.Join(shared, "agents.md"), "shared rules")

	// 1. First unlink should remove the base project copy and succeed
	if errno := root.Unlink(ctx, "agents.md"); errno != 0 {
		t.Fatalf("first unlink failed with errno %v", errno)
	}
	if _, err := os.Stat(filepath.Join(base, "agents.md")); !os.IsNotExist(err) {
		t.Fatalf("base copy still exists after unlink")
	}
	if _, err := os.Stat(filepath.Join(shared, "agents.md")); err != nil {
		t.Fatalf("shared overlay copy was removed: %v", err)
	}

	// 2. Second unlink should fail with EPERM because only overlay copy remains
	if errno := root.Unlink(ctx, "agents.md"); errno != syscall.EPERM {
		t.Fatalf("second unlink returned %v, want EPERM", errno)
	}
}

