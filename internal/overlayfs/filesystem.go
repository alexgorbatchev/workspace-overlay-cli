package overlayfs

import (
	"context"
	"errors"
	"log"
	"os"
	"path"
	"sort"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
)

type node struct {
	fs.Inode
	view *View
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
		var info os.FileInfo
		parts, err := n.view.resolve(path.Join(n.relativePath(), entry.Name()))
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Removed while the directory was being listed.
			continue
		case err != nil:
			// The entry stays unusable, but its siblings must remain listable.
			if info, err = entry.Info(); err != nil {
				continue
			}
		default:
			info = parts[len(parts)-1].info
		}
		var attr fuse.AttrOut
		attributes(info, &attr)
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
	if isGitCaller(ctx) {
		if parts[0].index == 0 {
			attributes(parts[0].info, out)
			return 0
		}
		return syscall.ENOENT
	}
	info := parts[len(parts)-1].info
	attributes(info, out)
	if info.Mode().IsRegular() && len(parts) > 1 {
		out.Size = uint64(mergedSize(parts))
		if rule := n.view.findMarkerRule(n.relativePath()); rule != nil {
			if data, err := n.renderMarkedData(parts, rule); err == nil {
				out.Size = uint64(len(data))
			}
		} else if notice, err := n.view.collisionNotice(n.relativePath(), parts); err == nil && notice != nil {
			out.Size = uint64(len(notice))
		}
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
	// A file contributed by one layer needs no merging. Serve it natively and
	// through the kernel page cache, so descriptor semantics, including shared
	// memory mappings, match the backing filesystem.
	if len(parts) == 1 || isGitCaller(ctx) {
		index := last.index
		if isGitCaller(ctx) {
			if parts[0].index != 0 {
				return nil, 0, syscall.ENOENT
			}
			index = 0
		}
		f, err := n.view.layers[index].root.OpenFile(n.relativePath(), int(flags&^(syscall.O_APPEND|fuse.FMODE_EXEC)), 0)
		if err != nil {
			return nil, 0, fs.ToErrno(err)
		}
		return fs.NewLoopbackFileFromOS(f), 0, 0
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

// Statfs reports the backing project's filesystem. Without it go-fuse answers
// with zeros, which tools read as a full disk.
func (n *node) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	dir, err := n.view.layers[0].root.Open(".")
	if err != nil {
		return fs.ToErrno(err)
	}
	defer logged.Close(dir)
	var stat syscall.Statfs_t
	if err := syscall.Fstatfs(int(dir.Fd()), &stat); err != nil {
		return fs.ToErrno(err)
	}
	out.FromStatfsT(&stat)
	return 0
}
