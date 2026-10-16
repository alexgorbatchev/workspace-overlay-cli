package overlayfs

import (
	"context"
	"os"
	"path"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
)

// NewListDirStream implements go-fuse's directory-handle interfaces, including
// seek and release. Return it directly instead of the bridge's lazy adapter.
func (n *node) OpendirHandle(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	stream, errno := n.Readdir(ctx)
	return stream, 0, errno
}

// directoryEntries uses native getdents types. Root.FS().ReadDir eagerly stats
// every entry to make DirEntry.Info safe after a mount hides the source path;
// FUSE enumeration needs only names and types, and Lookup provides attributes.
func (v *View) directoryEntries(name string) ([]fuse.DirEntry, error) {
	parts, err := v.resolve(name)
	if err != nil {
		return nil, err
	}
	if !parts[0].info.IsDir() {
		return nil, syscall.ENOTDIR
	}
	entries := make(map[string]fuse.DirEntry)
	var result []fuse.DirEntry
	for _, part := range parts {
		layer := &v.layers[part.index]
		file, err := layer.root.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		// The stream owns its descriptor. Duplicate atomically with CLOEXEC
		// while os.File retains ownership of the rooted open descriptor.
		fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		logged.Close(file)
		if err != nil {
			return nil, err
		}
		stream, errno := fs.NewLoopbackDirStreamFd(fd)
		if errno != 0 {
			_ = unix.Close(fd) // Preserve the stream-construction error.
			return nil, errno
		}
		for stream.HasNext() {
			entry, errno := stream.Next()
			if errno != 0 {
				stream.Close()
				return nil, errno
			}
			if entry.Name == "." || entry.Name == ".." {
				continue
			}
			if part.index > 0 {
				child := path.Join(name, entry.Name)
				if gitMetadata(child) || !layer.includes(child) {
					continue
				}
			}
			entry.Ino, entry.Off = 0, 0 // Lookup supplies the view's inode identity.
			if len(parts) == 1 {
				result = append(result, entry)
			} else {
				entries[entry.Name] = entry
			}
		}
		stream.Close()
	}
	if len(parts) == 1 {
		return result, nil
	}
	result = make([]fuse.DirEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	return result, nil
}
