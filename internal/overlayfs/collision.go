package overlayfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

// sniffLength is how much of a file http.DetectContentType inspects.
const sniffLength = 512

const noticeHeader = `workspace-overlay cannot show %q.

This path exists in more than one layer and at least one copy is a binary
file. Colliding text files are joined end to end, but binary files cannot be
joined, so this path shows this message until the collision is resolved.

Copies of this path:
`

const noticeFooter = `
Limitations:
  - No copy can be read or changed through this path while they collide.
    Each copy is intact at the location listed above.
  - A copy counts as binary when its first %d bytes do not look like text.

To resolve:
  - Keep one copy: delete or rename the others at the locations listed above.
  - Or narrow the overlay's "glob" in workspace-overlay.toml so that it no
    longer selects this path.
Then open the file again.
`

// binary reports whether a contribution cannot be joined with others as text.
func (v *View) binary(name string, part contribution) (bool, error) {
	f, err := v.layers[part.index].root.Open(name)
	if err != nil {
		return false, err
	}
	defer logged.Close(f)
	head := make([]byte, sniffLength)
	n, err := io.ReadFull(f, head)
	// A file shorter than the sniffed window is classified by what it holds.
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	return !strings.HasPrefix(http.DetectContentType(head[:n]), "text/"), nil
}

// collisionNotice returns the explanation served in place of a path whose
// copies cannot be merged, or nil when they merge as text.
func (v *View) collisionNotice(name string, parts []contribution) ([]byte, error) {
	if len(parts) < 2 {
		return nil, nil
	}
	var copies strings.Builder
	collides := false
	for _, part := range parts {
		binary, err := v.binary(name, part)
		if err != nil {
			return nil, err
		}
		layer, kind := "overlay", "text"
		if part.index == 0 {
			layer = "project"
		}
		if binary {
			kind, collides = "binary", true
		}
		location := pathname.Display(filepath.Join(v.layers[part.index].root.Name(), name))
		fmt.Fprintf(&copies, "  - %s (%s, %s)\n", location, layer, kind)
	}
	if !collides {
		return nil, nil
	}
	return []byte(fmt.Sprintf(noticeHeader, name) + copies.String() + fmt.Sprintf(noticeFooter, sniffLength)), nil
}

// noticeFile serves an explanation in place of a path whose copies cannot be
// merged. It rejects changes: nothing written here could be stored sensibly.
type noticeFile struct {
	data []byte
	attr fuse.Attr
}

var (
	_ fs.FileReader    = (*noticeFile)(nil)
	_ fs.FileGetattrer = (*noticeFile)(nil)
	_ fs.FileWriter    = (*noticeFile)(nil)
	_ fs.FileSetattrer = (*noticeFile)(nil)
	// Setattr by path opens a handle and releases it, so every handle Open
	// returns must be releasable.
	_ fs.FileReleaser = (*noticeFile)(nil)
)

func (f *noticeFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return nil, syscall.EINVAL
	}
	if off >= int64(len(f.data)) {
		return fuse.ReadResultData(nil), 0
	}
	end := min(off+int64(len(dest)), int64(len(f.data)))
	return fuse.ReadResultData(f.data[off:end]), 0
}

func (f *noticeFile) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	out.Attr = f.attr
	out.Size = uint64(len(f.data))
	return 0
}

func (f *noticeFile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	return 0, syscall.EPERM
}

func (f *noticeFile) Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return syscall.EPERM
}

func (f *noticeFile) Release(ctx context.Context) syscall.Errno {
	return 0
}
