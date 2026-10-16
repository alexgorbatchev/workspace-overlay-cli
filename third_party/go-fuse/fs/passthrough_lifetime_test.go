package fs

import (
	"os"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

type withoutPassthrough struct{ *LoopbackFile }

func (f *withoutPassthrough) PassthroughFd() (int, bool) { return -1, false }

func TestPassthroughDeclinedWithExistingBacking(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "backing")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	h := &withoutPassthrough{NewLoopbackFileFromOS(file)}
	// No registration is needed: another open already owns the inode's ID.
	b := rawBridge{server: &fuse.Server{}}
	n := Inode{backingID: 17, backingIDRefcount: 1}
	var out fuse.OpenOut
	b.addBackingID(&n, h, &out)
	if out.BackingID != 0 || out.OpenFlags&fuse.FOPEN_PASSTHROUGH != 0 || n.backingIDRefcount != 1 {
		t.Fatalf("declined handle borrowed an existing backing: out=%+v refs=%d", out, n.backingIDRefcount)
	}
}

func TestReleaseWithoutPassthroughPreservesBackingReferences(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "backing")
	if err != nil {
		t.Fatal(err)
	}
	n := &Inode{backingID: 17, backingIDRefcount: 2}
	b := rawBridge{kernelNodeIds: map[uint64]*Inode{2: n}, files: []*fileEntry{nil}}
	fe := b.registerFile(n, &withoutPassthrough{NewLoopbackFileFromOS(file)}, 0)
	b.Release(nil, &fuse.ReleaseIn{InHeader: fuse.InHeader{NodeId: 2}, Fh: uint64(fe.fh)})
	if n.backingIDRefcount != 2 {
		t.Fatalf("ordinary release removed another handle's reference: %d", n.backingIDRefcount)
	}
}
