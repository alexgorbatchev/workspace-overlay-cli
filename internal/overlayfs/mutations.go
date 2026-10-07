package overlayfs

import (
	"context"
	"errors"
	"log"
	"os"
	"path"
	"syscall"
	"time"
	"workspace-overlay/internal/logged"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func (n *node) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	n.view.mu.Lock()
	defer n.view.mu.Unlock()
	child := &node{view: n.view, path: path.Join(n.relativePath(), name)}
	index, err := n.view.destination(child.relativePath())
	if err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	if _, err := n.view.resolve(child.relativePath()); err == nil {
		return nil, nil, 0, syscall.EEXIST
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, 0, fs.ToErrno(err)
	}
	f, err := n.view.layers[index].root.OpenFile(child.relativePath(), int(flags&^(syscall.O_APPEND|fuse.FMODE_EXEC))|os.O_CREATE|os.O_EXCL, permissions(mode))
	if err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	if index > 0 {
		info, err := f.Stat()
		if err != nil {
			logged.Close(f)
			return nil, nil, 0, fs.ToErrno(err)
		}
		n.view.remember(index, info)
	}
	h := fs.NewLoopbackFileFromOS(f)
	if err := n.view.changed(index, child.relativePath()); err != nil {
		logged.Close(f)
		return nil, nil, 0, fs.ToErrno(err)
	}
	var attr fuse.AttrOut
	if errno := h.Getattr(ctx, &attr); errno != 0 {
		h.Release(ctx)
		return nil, nil, 0, errno
	}
	out.Attr = attr.Attr
	stable, err := n.view.stable(child.relativePath())
	if err != nil {
		h.Release(ctx)
		return nil, nil, 0, fs.ToErrno(err)
	}
	return n.NewInode(ctx, child, stable), h, 0, 0
}

func (n *node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.view.mu.Lock()
	defer n.view.mu.Unlock()
	child := path.Join(n.relativePath(), name)
	index, err := n.view.destination(child)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	if _, err := n.view.resolve(child); err == nil {
		return nil, syscall.EEXIST
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fs.ToErrno(err)
	}
	if err := n.view.layers[index].root.Mkdir(child, permissions(mode)); err != nil {
		return nil, fs.ToErrno(err)
	}
	if index > 0 {
		info, err := n.view.layers[index].root.Lstat(child)
		if err != nil {
			return nil, fs.ToErrno(err)
		}
		n.view.remember(index, info)
	}
	if err := n.view.changed(index, child); err != nil {
		return nil, fs.ToErrno(err)
	}
	return n.Lookup(ctx, name, out)
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	n.view.mu.Lock()
	defer n.view.mu.Unlock()
	child := path.Join(n.relativePath(), name)
	parts, err := n.view.resolve(child)
	if err != nil {
		return fs.ToErrno(err)
	}
	if parts[0].info.IsDir() {
		return syscall.EISDIR
	}
	last := parts[len(parts)-1]
	key := fileIdentity(last.index, last.info)
	// Overlay sources stay protected; only entries this session created may go.
	if last.index > 0 && (len(parts) != 1 || !n.view.created[key]) {
		return syscall.EPERM
	}
	if err := n.view.layers[last.index].root.Remove(child); err != nil {
		return fs.ToErrno(err)
	}
	delete(n.view.created, key)
	return fs.ToErrno(n.view.changed(last.index, child))
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.view.mu.Lock()
	defer n.view.mu.Unlock()
	child := path.Join(n.relativePath(), name)
	entries, err := n.view.entries(child)
	if err != nil {
		return fs.ToErrno(err)
	}
	if len(entries) != 0 {
		return syscall.ENOTEMPTY
	}
	parts, err := n.view.resolve(child)
	if err != nil {
		return fs.ToErrno(err)
	}
	// Removing only one of several directories would falsely report successful removal.
	key := fileIdentity(parts[0].index, parts[0].info)
	if len(parts) != 1 || (parts[0].index > 0 && !n.view.created[key]) {
		return syscall.EPERM
	}
	if err := n.view.layers[parts[0].index].root.Remove(child); err != nil {
		return fs.ToErrno(err)
	}
	delete(n.view.created, key)
	return fs.ToErrno(n.view.changed(parts[0].index, child))
}

func (n *node) Setattr(ctx context.Context, handle fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if h, ok := handle.(fs.FileSetattrer); ok {
		return h.Setattr(ctx, in, out)
	}
	parts, err := n.view.resolve(n.relativePath())
	if err != nil {
		return fs.ToErrno(err)
	}
	// Only size changes on regular files need to open through the handle
	// to apply staged-content rules for merged files.
	_, sizeSet := in.GetSize()
	if parts[0].info.Mode().IsRegular() && sizeSet {
		h, _, errno := n.Open(ctx, syscall.O_RDWR)
		if errno != 0 {
			return errno
		}
		setter := h.(fs.FileSetattrer)
		errno = setter.Setattr(ctx, in, out)
		if release := h.(fs.FileReleaser).Release(ctx); errno == 0 {
			errno = release
		}
		return errno
	}
	root := n.view.layers[parts[len(parts)-1].index].root
	uid, uok := in.GetUID()
	gid, gok := in.GetGID()
	if uok || gok {
		owner, group := -1, -1
		if uok {
			owner = int(uid)
		}
		if gok {
			group = int(gid)
		}
		if err := root.Lchown(n.relativePath(), owner, group); err != nil {
			return fs.ToErrno(err)
		}
	}
	if mode, ok := in.GetMode(); ok {
		if err := root.Chmod(n.relativePath(), permissions(mode)); err != nil {
			return fs.ToErrno(err)
		}
	}
	atime, aok := in.GetATime()
	mtime, mok := in.GetMTime()
	if aok || mok {
		info := parts[len(parts)-1].info
		stat := info.Sys().(*syscall.Stat_t)
		if !aok {
			atime = time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
		}
		if !mok {
			mtime = info.ModTime()
		}
		if err := root.Chtimes(n.relativePath(), atime, mtime); err != nil {
			return fs.ToErrno(err)
		}
	}
	return n.Getattr(ctx, nil, out)
}

func (n *node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.view.mu.Lock()
	defer n.view.mu.Unlock()
	child := path.Join(n.relativePath(), name)
	index, err := n.view.destination(child)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	if err := n.view.layers[index].root.Symlink(target, child); err != nil {
		return nil, fs.ToErrno(err)
	}
	if index > 0 {
		info, err := n.view.layers[index].root.Lstat(child)
		if err != nil {
			return nil, fs.ToErrno(err)
		}
		n.view.remember(index, info)
	}
	if err := n.view.changed(index, child); err != nil {
		return nil, fs.ToErrno(err)
	}
	return n.Lookup(ctx, name, out)
}

func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if flags != 0 {
		return syscall.ENOTSUP
	}
	dest, ok := newParent.(*node)
	if !ok || dest.view != n.view {
		return syscall.EXDEV
	}
	n.view.mu.Lock()
	defer n.view.mu.Unlock()
	sourcePath, destPath := path.Join(n.relativePath(), name), path.Join(dest.relativePath(), newName)
	if sourcePath == destPath {
		return 0
	}
	parts, err := n.view.resolve(sourcePath)
	if err != nil {
		return fs.ToErrno(err)
	}
	// A directory spread across layers cannot be moved by one native rename.
	if parts[0].info.IsDir() && len(parts) > 1 {
		return syscall.EXDEV
	}
	sourceIndex := parts[len(parts)-1].index
	key := fileIdentity(sourceIndex, parts[len(parts)-1].info)
	destExisted := false
	if sourceIndex > 0 {
		// Existing overlay paths stay protected against moves and deletion.
		// Entries this session created may move to a free name, and a staging
		// file may replace an overlay document (an editor's atomic save).
		if !n.view.created[key] {
			return syscall.EPERM
		}
		destParts, destErr := n.view.resolve(destPath)
		if destErr != nil && !errors.Is(destErr, os.ErrNotExist) {
			return fs.ToErrno(destErr)
		}
		destExisted = destErr == nil
		if destExisted && (parts[0].info.IsDir() || !destParts[0].info.Mode().IsRegular() || destParts[len(destParts)-1].index == 0) {
			return syscall.EPERM
		}
	}
	destIndex, err := n.view.destination(destPath)
	if err != nil {
		return fs.ToErrno(err)
	}
	original, errno := n.prepareSave(sourceIndex, sourcePath, destPath)
	if errno != 0 {
		return errno
	}
	errno = renameBetween(n.view.layers[sourceIndex].root, sourcePath, n.view.layers[destIndex].root, destPath)
	if errno != 0 && original != nil {
		if err := n.view.layers[sourceIndex].root.WriteFile(sourcePath, original, parts[len(parts)-1].info.Mode().Perm()); err != nil {
			log.Printf("restore unsuccessful save: %v", err)
			return syscall.EIO
		}
	}
	if errno == 0 {
		delete(n.view.created, key)
		// A replaced document is protected again; a moved entry stays removable.
		if sourceIndex > 0 && !destExisted && destIndex > 0 {
			n.view.created[identity{layer: destIndex, device: key.device, inode: key.inode}] = true
		}
		return fs.ToErrno(n.view.changed(max(sourceIndex, destIndex), sourcePath, destPath))
	}
	return errno
}

func (n *node) prepareSave(index int, source, dest string) ([]byte, syscall.Errno) {
	parts, err := n.view.resolve(dest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0
	}
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	if !parts[0].info.Mode().IsRegular() || len(parts) == 1 {
		return nil, 0
	}
	notice, err := n.view.collisionNotice(dest, parts)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	// A path that shows an explanation has no content a save could replace.
	if notice != nil {
		return nil, syscall.EPERM
	}
	prefix, err := n.view.contents(dest, parts[:len(parts)-1])
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	root := n.view.layers[index].root
	data, err := root.ReadFile(source)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	original := data
	data, err = contributionBytes(data, prefix)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	return original, fs.ToErrno(root.WriteFile(source, data, parts[len(parts)-1].info.Mode().Perm()))
}

func (n *node) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	source, ok := target.(*node)
	if !ok || source.view != n.view {
		return nil, syscall.EXDEV
	}
	n.view.mu.Lock()
	defer n.view.mu.Unlock()
	parts, err := n.view.resolve(source.relativePath())
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	if len(parts) != 1 {
		return nil, syscall.EXDEV
	}
	root := n.view.layers[parts[0].index].root
	if err := root.Link(source.relativePath(), path.Join(n.relativePath(), name)); err != nil {
		return nil, fs.ToErrno(err)
	}
	if err := n.view.changed(parts[0].index, source.relativePath(), path.Join(n.relativePath(), name)); err != nil {
		return nil, fs.ToErrno(err)
	}
	return n.Lookup(ctx, name, out)
}

func renameBetween(sourceRoot *os.Root, source string, destRoot *os.Root, dest string) syscall.Errno {
	sourceDir, err := sourceRoot.Open(path.Dir(source))
	if err != nil {
		return fs.ToErrno(err)
	}
	defer logged.Close(sourceDir)
	destDir, err := destRoot.Open(path.Dir(dest))
	if err != nil {
		return fs.ToErrno(err)
	}
	defer logged.Close(destDir)
	return fs.ToErrno(syscall.Renameat(int(sourceDir.Fd()), path.Base(source), int(destDir.Fd()), path.Base(dest)))
}

func permissions(mode uint32) os.FileMode {
	result := os.FileMode(mode & 0777)
	if mode&syscall.S_ISUID != 0 {
		result |= os.ModeSetuid
	}
	if mode&syscall.S_ISGID != 0 {
		result |= os.ModeSetgid
	}
	if mode&syscall.S_ISVTX != 0 {
		result |= os.ModeSticky
	}
	return result
}
