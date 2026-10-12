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
	if errno := child.prepareGetattr(child.path); errno != 0 {
		return nil, errno
	}
	cacheable := n.view.cache.watch(n, name)
	parts, err := n.view.resolve(child.path)
	if err != nil {
		if cacheable && errors.Is(err, os.ErrNotExist) && n.view.projectNamespace(child.path) {
			out.SetEntryTimeout(metadataTimeout)
		}
		return nil, fs.ToErrno(err)
	}
	var attr fuse.AttrOut
	if errno := child.getattr(ctx, child.path, parts, &attr); errno != 0 {
		return nil, errno
	}
	out.Attr = attr.Attr
	// Both callers see an entry when the project contributes, even when
	// its attributes differ. Overlay-only entries stay caller-dependent.
	if cacheable && parts[0].index == 0 {
		out.SetEntryTimeout(metadataTimeout)
		if parts[0].info.Mode().IsRegular() && n.view.projectNamespace(child.path) {
			out.SetAttrTimeout(metadataTimeout)
		}
	}
	stable := n.view.stablePart(parts[len(parts)-1])
	return n.NewInode(ctx, child, stable), 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if err := n.view.refreshExclude(); err != nil {
		log.Printf("refresh Git exclusions: %v", err)
		return nil, syscall.EIO
	}
	result, err := n.view.directoryEntries(n.relativePath())
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return fs.NewListDirStream(result), 0
}

func (n *node) Getattr(ctx context.Context, handle fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	name := n.relativePath()
	if errno := n.prepareGetattr(name); errno != 0 {
		return errno
	}
	if h, ok := handle.(fs.FileGetattrer); ok {
		return h.Getattr(ctx, out)
	}
	parts, err := n.view.resolve(name)
	if err != nil {
		return fs.ToErrno(err)
	}
	errno := n.getattr(ctx, name, parts, out)
	if errno == 0 && parts[0].index == 0 && parts[0].info.Mode().IsRegular() && n.view.projectNamespace(name) && n.Operations() != nil {
		name, parent := n.Parent()
		if parent != nil && n.view.cache.watch(parent.Operations().(*node), name) {
			out.SetTimeout(metadataTimeout)
		}
	}
	return errno
}

func (n *node) prepareGetattr(name string) syscall.Errno {
	// Git inspects its metadata before loading exclusions and enumerating the worktree.
	if name == "." || name == ".git" || name == ".git/info/exclude" {
		if err := n.view.refreshExclude(); err != nil {
			log.Printf("refresh Git exclusions: %v", err)
			return syscall.EIO
		}
	}
	return 0
}

func (n *node) getattr(ctx context.Context, name string, parts []contribution, out *fuse.AttrOut) syscall.Errno {
	if callerDependent(parts) && isGitCaller(ctx) {
		if parts[0].index == 0 {
			attributes(parts[0].info, out)
			return 0
		}
		return syscall.ENOENT
	}
	// Path stats must see staged writes without borrowing an unrelated
	// reader's snapshot. Git's project view was handled above.
	for _, writer := range n.view.writers(name) {
		if writer.pendingAttributes(out) {
			return 0
		}
	}
	info := parts[len(parts)-1].info
	attributes(info, out)
	if !info.Mode().IsRegular() {
		return 0
	}
	// Classify the real copies before adding an empty project section. Open
	// serves the collision explanation regardless of the file's marker rule.
	if notice, err := n.view.collisionNotice(name, parts); err == nil && notice != nil {
		out.Size = uint64(len(notice))
		return 0
	}
	rule := n.view.findMarkerRule(name)
	if rule != nil {
		parts = n.view.document(name, parts)
	}
	if len(parts) > 1 {
		// A merged file stands for one file in each layer. Reporting them as
		// links tells programs that replacing the file would cut it off from
		// the others, so editors such as Vim write it in place instead.
		out.Nlink = uint32(len(parts))
		out.Size = uint64(mergedSize(parts))
		if rule != nil {
			if size, err := n.markedSize(name, parts, rule); err == nil {
				out.Size = uint64(size)
			}
		}
	}
	return 0
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.view.mu.Lock()
	defer n.view.mu.Unlock()
	name := n.relativePath()
	parts, err := n.view.resolve(name)
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
	git := callerDependent(parts) && isGitCaller(ctx)
	if !git && n.view.findMarkerRule(name) != nil {
		parts = n.view.document(name, parts)
	}
	if len(parts) == 1 || git {
		index := last.index
		if git {
			if parts[0].index != 0 {
				return nil, 0, syscall.ENOENT
			}
			index = 0
		}
		f, err := n.view.layers[index].root.OpenFile(name, int(flags&^(syscall.O_APPEND|fuse.FMODE_EXEC)), 0)
		if err != nil {
			return nil, 0, fs.ToErrno(err)
		}
		return fs.NewLoopbackFileFromOS(f), 0, 0
	}
	return n.openMerged(ctx, flags, parts)
}

func (n *node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	name := n.relativePath()
	parts, err := n.view.resolve(name)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	target, err := n.view.layers[parts[len(parts)-1].index].root.Readlink(name)
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
