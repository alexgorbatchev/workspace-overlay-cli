package overlayfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestPathStatWithAnotherCallersOpenHandle(t *testing.T) {
	original := readProcessComm
	t.Cleanup(func() { readProcessComm = original })
	readProcessComm = func(pid uint32) string {
		if pid == 1 {
			return "git"
		}
		return "editor"
	}
	for _, opener := range []uint32{1, 2} {
		t.Run(readProcessComm(opener), func(t *testing.T) {
			v, project, source, _ := setupTestView(t)
			scratch.Write(t, filepath.Join(project, "merged.txt"), "base")
			scratch.Write(t, filepath.Join(source, "merged.txt"), "overlay")
			raw := fs.NewNodeFS(&node{view: v, path: "."}, nil)
			var entry fuse.EntryOut
			header := fuse.InHeader{NodeId: 1, Caller: fuse.Caller{Pid: opener}}
			if status := raw.Lookup(nil, &header, "merged.txt", &entry); status != fuse.OK {
				t.Fatal(status)
			}
			header.NodeId = entry.NodeId
			var opened fuse.OpenOut
			if status := raw.Open(nil, &fuse.OpenIn{InHeader: header, Flags: syscall.O_RDWR}, &opened); status != fuse.OK {
				t.Fatal(status)
			}
			t.Cleanup(func() { raw.Release(nil, &fuse.ReleaseIn{InHeader: header, Fh: opened.Fh}) })
			for _, caller := range []uint32{1, 2} {
				in := fuse.GetAttrIn{InHeader: fuse.InHeader{NodeId: entry.NodeId, Caller: fuse.Caller{Pid: caller}}}
				var out fuse.AttrOut
				want := uint64(len("baseoverlay"))
				if caller == 1 {
					want = uint64(len("base"))
				}
				if status := raw.GetAttr(nil, &in, &out); status != fuse.OK || out.Size != want {
					t.Errorf("path stat by %s with %s handle: status=%v size=%d, want %d", readProcessComm(caller), readProcessComm(opener), status, out.Size, want)
				}
			}
			// Descriptor stats must still use the actual open file: it can
			// have staged writes, or survive unlinking its backing pathname.
			want := uint64(len("base"))
			if opener == 1 {
				if err := os.Remove(filepath.Join(project, "merged.txt")); err != nil {
					t.Fatal(err)
				}
			} else {
				data := []byte("extended")
				written, status := raw.Write(nil, &fuse.WriteIn{InHeader: header, Fh: opened.Fh, Offset: uint64(len("baseoverlay"))}, data)
				if status != fuse.OK || written != uint32(len(data)) {
					t.Fatalf("write: status=%v count=%d", status, written)
				}
				want = uint64(len("baseoverlayextended"))
				var pending fuse.AttrOut
				if status := raw.GetAttr(nil, &fuse.GetAttrIn{InHeader: header}, &pending); status != fuse.OK || pending.Size != want {
					t.Errorf("path stat after staged write: status=%v size=%d, want %d", status, pending.Size, want)
				}
			}
			in := fuse.GetAttrIn{InHeader: header, Flags_: fuse.FUSE_GETATTR_FH, Fh_: opened.Fh}
			var out fuse.AttrOut
			if status := raw.GetAttr(nil, &in, &out); status != fuse.OK || out.Size != want {
				t.Errorf("descriptor stat: status=%v size=%d, want %d", status, out.Size, want)
			}
		})
	}
}
