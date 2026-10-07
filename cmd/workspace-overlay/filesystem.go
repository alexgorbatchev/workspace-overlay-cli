package main

import (
	"context"
	"log"
	"os"
	"path"
	"sort"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type node struct {
	fs.Inode
	view *view
	path string
}

func (n *node) relativePath() string {
	// Before publication and at a subtree root, path identifies the backing location.
	if n.Operations() == nil || n.IsRoot() {
		return n.path
	}
	// go-fuse updates inode ancestry on rename and hard link operations.
	root := n.Root()
	return path.Join(root.Operations().(*node).path, n.Path(root))
}

func attributes(info os.FileInfo, out *fuse.AttrOut) {
	out.FromStat(info.Sys().(*syscall.Stat_t))
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	child := &node{view: n.view, path: path.Join(n.relativePath(), name)}
	var attr fuse.AttrOut
	if errno := child.Getattr(ctx, nil, &attr); errno != 0 {
		return nil, errno
	}
	out.Attr = attr.Attr
	stable, err := n.view.stable(child.relativePath())
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	return n.NewInode(ctx, child, stable), 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if err := n.view.refreshExclude(); err != nil {
		log.Printf("refresh Git exclusions: %v", err)
		return nil, syscall.EIO
	}
	entries, err := n.view.entries(n.relativePath())
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	result := make([]fuse.DirEntry, 0, len(entries))
	for _, entry := range entries {
		parts, err := n.view.resolve(path.Join(n.relativePath(), entry.Name()))
		if err != nil {
			return nil, fs.ToErrno(err)
		}
		var attr fuse.AttrOut
		attributes(parts[len(parts)-1].info, &attr)
		result = append(result, fuse.DirEntry{Name: entry.Name(), Mode: attr.Mode})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return fs.NewListDirStream(result), 0
}

func (n *node) Getattr(ctx context.Context, handle fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	// Git inspects its metadata before loading exclusions and enumerating the worktree.
	if n.relativePath() == "." || n.relativePath() == ".git" || n.relativePath() == ".git/info/exclude" {
		if err := n.view.refreshExclude(); err != nil {
			log.Printf("refresh Git exclusions: %v", err)
			return syscall.EIO
		}
	}
	if h, ok := handle.(fs.FileGetattrer); ok {
		return h.Getattr(ctx, out)
	}
	parts, err := n.view.resolve(n.relativePath())
	if err != nil {
		return fs.ToErrno(err)
	}
	info := parts[len(parts)-1].info
	attributes(info, out)
	if info.Mode().IsRegular() && (len(parts) > 1 || parts[0].index != 0) {
		data, err := n.view.contents(n.relativePath(), parts)
		if err != nil {
			log.Printf("render %s: %v", n.relativePath(), err)
			return syscall.EIO
		}
		out.Size = uint64(len(data))
	}
	return 0
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.view.mu.Lock()
	defer n.view.mu.Unlock()
	parts, err := n.view.resolve(n.relativePath())
	if err != nil {
		return nil, 0, fs.ToErrno(err)
	}
	if parts[0].info.IsDir() {
		return nil, 0, syscall.EISDIR
	}
	last := parts[len(parts)-1]
	if !last.info.Mode().IsRegular() {
		return nil, 0, syscall.ENOTSUP
	}
	// Ordinary files retain native descriptor semantics, including writes and fsync.
	if len(parts) == 1 && last.index == 0 {
		f, err := n.view.layers[0].root.OpenFile(n.relativePath(), int(flags&^(syscall.O_APPEND|fuse.FMODE_EXEC)), 0)
		if err != nil {
			return nil, 0, fs.ToErrno(err)
		}
		return fs.NewLoopbackFileFromOS(f), fuse.FOPEN_DIRECT_IO, 0
	}
	return n.openMerged(ctx, flags, parts)
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	parts, err := n.view.resolve(n.relativePath())
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	target, err := n.view.layers[parts[len(parts)-1].index].root.Readlink(n.relativePath())
	return []byte(target), fs.ToErrno(err)
}
