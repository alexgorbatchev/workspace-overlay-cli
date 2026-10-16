package overlayfs

import (
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// nativeFile retains loopback descriptor semantics but only offers kernel
// passthrough for a permanently caller-invariant backing file. go-fuse handles
// capability negotiation and falls back when backing registration fails.
type nativeFile struct {
	*fs.LoopbackFile
	passthrough bool
}

func (f *nativeFile) PassthroughFd() (int, bool) {
	if !f.passthrough {
		return -1, false
	}
	return f.LoopbackFile.PassthroughFd()
}

// nativeOpen is called with view.mu held. Checking the opened descriptor
// catches replacement between resolve and open. Ctime catches equal-size
// external edits even when the writer restores mtime. A changed version uses
// the normal OPEN response, which discards cached pages before the next read.
func (n *node) nativeOpen(f *os.File, index int, name string, flags uint32) (*nativeFile, uint32) {
	// Open has resolved the current contributions. Data can be reused for a
	// project-only file even under a broad glob; a later merged open uses direct
	// I/O. Passthrough remains restricted to permanently invariant namespaces.
	invariant := index == 0
	var info os.FileInfo
	if invariant {
		info, _ = f.Stat() // A failed stat falls back to uncached descriptor I/O.
		if info == nil || (n.Operations() != nil && n.StableAttr().Ino != n.view.stablePart(contribution{index: index, info: info}).Ino) {
			invariant = false
		}
	}
	h := &nativeFile{LoopbackFile: fs.NewLoopbackFileFromOS(f), passthrough: invariant && n.view.projectNamespace(name)}
	var openFlags uint32
	if invariant && n.Operations() != nil && flags&(syscall.O_ACCMODE|syscall.O_TRUNC) == syscall.O_RDONLY {
		entry, parent := n.Parent()
		if parent != nil && n.view.cache.watch(parent.Operations().(*node), entry) {
			stat := info.Sys().(*syscall.Stat_t)
			version := nativeVersion{device: uint64(stat.Dev), inode: stat.Ino, size: stat.Size, mtime: stat.Mtim, ctime: stat.Ctim}
			if n.version == version {
				openFlags = fuse.FOPEN_KEEP_CACHE
			}
			n.version = version
			n.dataCached.Store(true)
			return h, openFlags
		}
	}
	n.version = nativeVersion{}
	return h, openFlags
}

type nativeVersion struct {
	device, inode uint64
	size          int64
	mtime, ctime  syscall.Timespec
}
