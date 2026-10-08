package overlayfs

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
)

// Merged files are staged in memory, so their size is bounded.
const maxMergedSize = 64 << 20

func mergedSize(parts []contribution) int64 {
	var size int64
	for _, part := range parts {
		size += part.info.Size()
	}
	return size
}

// A merged file stages logical edits, then saves only the most-specific contribution.
// Earlier contributions must remain byte-for-byte intact in a full-document save.
type mergedFile struct {
	mu                                    sync.Mutex
	node                                  *node
	parts                                 []contribution
	markerRule                            *MarkerRule
	file                                  *os.File
	prefix, data                          []byte
	attr                                  fuse.Attr
	failed                                syscall.Errno
	readable, writable, appendMode, dirty bool
}

func (n *node) renderMarkedData(parts []contribution, rule *MarkerRule) ([]byte, error) {
	var buf bytes.Buffer
	baseBytes, err := n.view.layers[parts[0].index].root.ReadFile(n.relativePath())
	if err != nil {
		return nil, err
	}
	buf.Write(baseBytes)

	for i := 1; i < len(parts); i++ {
		l := n.view.layers[parts[i].index]
		name := l.name
		if name == "" {
			name = filepath.Base(l.root.Name())
		}
		relPath, err := filepath.Rel(n.view.layers[0].root.Name(), filepath.Join(l.root.Name(), n.relativePath()))
		if err != nil {
			relPath = filepath.Join(l.root.Name(), n.relativePath())
		}

		start := formatMarker(rule.Start, name, relPath)
		end := formatMarker(rule.End, name, relPath)

		overlayBytes, err := l.root.ReadFile(n.relativePath())
		if err != nil {
			return nil, err
		}

		if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
			buf.WriteByte('\n')
		}
		buf.WriteString(start)
		buf.WriteByte('\n')
		buf.Write(overlayBytes)
		if len(overlayBytes) > 0 && overlayBytes[len(overlayBytes)-1] != '\n' {
			buf.WriteByte('\n')
		}
		buf.WriteString(end)
		buf.WriteByte('\n')
	}

	return buf.Bytes(), nil
}

func (n *node) openMerged(ctx context.Context, flags uint32, parts []contribution) (fs.FileHandle, uint32, syscall.Errno) {
	last := parts[len(parts)-1]
	notice, err := n.view.collisionNotice(n.relativePath(), parts)
	if err != nil {
		log.Printf("render %s: %v", n.relativePath(), err)
		return nil, 0, syscall.EIO
	}
	// Binary copies are never joined, whatever their size, so this comes first.
	if notice != nil {
		log.Printf("cannot merge %s: binary collision; serving an explanation instead of file content", n.relativePath())
		var attr fuse.AttrOut
		attributes(last.info, &attr)
		return &noticeFile{data: notice, attr: attr.Attr}, fuse.FOPEN_DIRECT_IO, 0
	}
	rule := n.view.findMarkerRule(n.relativePath())
	var prefix, data []byte
	if rule == nil {
		if mergedSize(parts) > maxMergedSize {
			return nil, 0, syscall.EFBIG
		}
		prefix, err = n.view.contents(n.relativePath(), parts[:len(parts)-1])
		if err != nil {
			log.Printf("render %s: %v", n.relativePath(), err)
			return nil, 0, syscall.EIO
		}
		final, err := n.view.layers[last.index].root.ReadFile(n.relativePath())
		if err != nil {
			log.Printf("render %s: %v", n.relativePath(), err)
			return nil, 0, syscall.EIO
		}
		data = append(bytes.Clone(prefix), final...)
	} else {
		data, err = n.renderMarkedData(parts, rule)
		if err != nil {
			log.Printf("render marked %s: %v", n.relativePath(), err)
			return nil, 0, syscall.EIO
		}
		if int64(len(data)) > maxMergedSize {
			return nil, 0, syscall.EFBIG
		}
	}
	f, err := n.view.layers[last.index].root.OpenFile(n.relativePath(), int(flags&^(syscall.O_TRUNC|syscall.O_APPEND|fuse.FMODE_EXEC)), 0)
	if err != nil {
		return nil, 0, fs.ToErrno(err)
	}
	var attr fuse.AttrOut
	attributes(last.info, &attr)
	h := &mergedFile{
		node:       n,
		parts:      parts,
		markerRule: rule,
		file:       f,
		prefix:     prefix,
		data:       data,
		attr:       attr.Attr,
		readable:   flags&syscall.O_ACCMODE != syscall.O_WRONLY,
		writable:   flags&syscall.O_ACCMODE != syscall.O_RDONLY,
		appendMode: flags&syscall.O_APPEND != 0,
	}
	if flags&syscall.O_TRUNC != 0 {
		h.data = nil
		h.dirty = true
	}
	return h, fuse.FOPEN_DIRECT_IO, 0
}

func (f *mergedFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.readable {
		return nil, syscall.EBADF
	}
	if off < 0 {
		return nil, syscall.EINVAL
	}
	if off >= int64(len(f.data)) {
		return fuse.ReadResultData(nil), 0
	}
	end := min(off+int64(len(dest)), int64(len(f.data)))
	// go-fuse consumes the result after this method returns; copy to avoid write races.
	return fuse.ReadResultData(bytes.Clone(f.data[off:end])), 0
}

func (f *mergedFile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.writable {
		return 0, syscall.EBADF
	}
	if f.appendMode {
		off = int64(len(f.data))
	}
	if off < 0 || off > maxMergedSize || int64(len(data)) > maxMergedSize-off {
		return 0, syscall.EFBIG
	}
	end := int(off) + len(data)
	if f.markerRule == nil && off < int64(len(f.prefix)) {
		prefixEnd := min(end, len(f.prefix))
		if !bytes.Equal(data[:prefixEnd-int(off)], f.prefix[int(off):prefixEnd]) {
			f.failed = syscall.EPERM
			return 0, f.failed
		}
	}
	if end > len(f.data) {
		f.data = append(f.data, make([]byte, end-len(f.data))...)
	}
	copy(f.data[int(off):], data)
	f.dirty = true
	return uint32(len(data)), 0
}

func (f *mergedFile) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	out.Attr = f.attr
	out.Size = uint64(len(f.data))
	return 0
}

func (f *mergedFile) commit() syscall.Errno {
	if f.failed != 0 {
		return f.failed
	}
	if !f.dirty {
		return 0
	}
	if f.markerRule != nil {
		if len(f.data) == 0 {
			return 0
		}
		sections, err := parseMarkedDocument(f.data, f.parts, f.node.view.layers, f.markerRule)
		if err != nil {
			log.Printf("save marked merged file: %v (len data=%d)", err, len(f.data))
			return fs.ToErrno(err)
		}
		for _, sec := range sections {
			layer := f.node.view.layers[sec.index]
			origBytes, err := layer.root.ReadFile(f.node.relativePath())
			if err != nil {
				return fs.ToErrno(err)
			}
			if !bytes.Equal(sec.data, origBytes) {
				file, err := layer.root.OpenFile(f.node.relativePath(), os.O_WRONLY|os.O_TRUNC, 0)
				if err != nil {
					return fs.ToErrno(err)
				}
				if _, err := file.Write(sec.data); err != nil {
					logged.Close(file)
					return fs.ToErrno(err)
				}
				if err := file.Close(); err != nil {
					return fs.ToErrno(err)
				}
			}
		}
		f.dirty = false
		return 0
	}

	data, err := contributionBytes(f.data, f.prefix)
	if err != nil {
		log.Printf("save merged file: %v", err)
		return fs.ToErrno(err)
	}
	if _, err := f.file.WriteAt(data, 0); err != nil {
		return fs.ToErrno(err)
	}
	if err := f.file.Truncate(int64(len(data))); err != nil {
		return fs.ToErrno(err)
	}
	f.data = append(bytes.Clone(f.prefix), data...)
	info, err := f.file.Stat()
	if err != nil {
		return fs.ToErrno(err)
	}
	var attr fuse.AttrOut
	attributes(info, &attr)
	f.attr = attr.Attr
	f.dirty = false
	return 0
}

func contributionBytes(data, prefix []byte) ([]byte, error) {
	// Truncation clears the writable contribution, while earlier layers stay visible.
	if len(data) == 0 {
		return nil, nil
	}
	if !bytes.HasPrefix(data, prefix) {
		return nil, syscall.EPERM
	}
	return data[len(prefix):], nil
}

func (f *mergedFile) Flush(ctx context.Context) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commit()
}

func (f *mergedFile) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	if errno := f.commit(); errno != 0 {
		return errno
	}
	return fs.ToErrno(f.file.Sync())
}

func (f *mergedFile) Release(ctx context.Context) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	errno := f.commit()
	err := f.file.Close()
	if errno != 0 {
		return errno
	}
	return fs.ToErrno(err)
}

func (f *mergedFile) Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	if size, ok := in.GetSize(); ok {
		if !f.writable {
			return syscall.EBADF
		}
		if size > maxMergedSize {
			return syscall.EFBIG
		}
		if int(size) < len(f.data) {
			f.data = f.data[:int(size)]
		} else {
			f.data = append(f.data, make([]byte, int(size)-len(f.data))...)
		}
		f.dirty = true
	}
	// Delegate metadata changes through a duplicate descriptor; buffer virtual size separately.
	fd, err := syscall.Dup(int(f.file.Fd()))
	if err != nil {
		return fs.ToErrno(err)
	}
	native := fs.NewLoopbackFileFromOS(os.NewFile(uintptr(fd), f.file.Name()))
	metadata := *in
	metadata.Valid &^= fuse.FATTR_SIZE
	var attr fuse.AttrOut
	errno := native.Setattr(ctx, &metadata, &attr)
	if release := native.Release(ctx); errno == 0 {
		errno = release
	}
	if errno != 0 {
		return errno
	}
	f.attr = attr.Attr
	out.Attr = f.attr
	out.Size = uint64(len(f.data))
	return 0
}
