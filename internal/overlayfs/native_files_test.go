package overlayfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestSingleContributionFileUsesNativeHandle(t *testing.T) {
	root, _, shared, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(shared, "only.txt"), "old")
	child := &node{view: root.view, path: "only.txt"}

	ctx := context.Background()
	h, flags, errno := child.Open(ctx, syscall.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open returned errno %v", errno)
	}
	if _, ok := h.(*mergedFile); ok {
		t.Fatalf("single contribution file should not be *mergedFile")
	}
	if flags != 0 {
		t.Errorf("flags = %v, want 0", flags)
	}

	written, errno := h.(fs.FileWriter).Write(ctx, []byte("new"), 0)
	if errno != 0 {
		t.Fatalf("Write returned errno %v", errno)
	}
	if written != 3 {
		t.Errorf("wrote %d bytes, want 3", written)
	}

	if errno := h.(fs.FileReleaser).Release(ctx); errno != 0 {
		t.Fatalf("Release errno = %v", errno)
	}
	if got := string(scratch.Read(t, filepath.Join(shared, "only.txt"))); got != "new" {
		t.Errorf("shared file = %q, want %q", got, "new")
	}
}

func TestMergedSizeNeedsNoRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test requires non-root user")
	}
	root, base, shared, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(base, "doc.txt"), "BASE-")
	scratch.Write(t, filepath.Join(shared, "doc.txt"), "SHARED")
	sharedPath := filepath.Join(shared, "doc.txt")
	if err := os.Chmod(sharedPath, 0); err != nil {
		t.Fatal(err)
	}
	child := &node{view: root.view, path: "doc.txt"}
	var out fuse.AttrOut
	errno := child.Getattr(context.Background(), nil, &out)
	if errno != 0 {
		t.Fatalf("Getattr returned errno %v (want 0)", errno)
	}
	// Size should be 5 + 6 = 11.
	if out.Size != 11 {
		t.Errorf("Size = %d, want 11", out.Size)
	}
}

func TestOversizedMergedFileIsRefused(t *testing.T) {
	n, baseFile, _ := setupMergedNode(t, "BASE-", "OVERLAY")
	// A text head keeps the copy mergeable as text; the sparse tail pushes the
	// merged size past the 64 MiB limit without writing real data.
	scratch.Write(t, baseFile, strings.Repeat("a", 512))
	if err := os.Truncate(baseFile, 64<<20+1); err != nil {
		t.Fatal(err)
	}
	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}
	h, _, errno := n.openMerged(context.Background(), syscall.O_RDONLY, parts)
	if h != nil {
		if rel, ok := h.(fs.FileReleaser); ok {
			if errno := rel.Release(context.Background()); errno != 0 {
				t.Errorf("Release errno = %v", errno)
			}
		}
	}
	if errno != syscall.EFBIG {
		t.Errorf("openMerged errno = %v, want EFBIG", errno)
	}
}

func TestMergedFileRejectsGrowthBeyondLimit(t *testing.T) {
	n, _, _ := setupMergedNode(t, "BASE-", "OVERLAY")
	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	handle := h.(*mergedFile)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
		if errno := handle.Release(ctx); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	_, errWrite := handle.Write(ctx, []byte("x"), 1<<62)
	if errWrite != syscall.EFBIG {
		t.Errorf("Write at huge offset returned errno %v, want EFBIG", errWrite)
	}

	var in fuse.SetAttrIn
	in.Size = 1 << 62
	in.Valid |= fuse.FATTR_SIZE
	var out fuse.AttrOut
	errSetattr := handle.Setattr(ctx, &in, &out)
	if errSetattr != syscall.EFBIG {
		t.Errorf("Setattr with huge size returned errno %v, want EFBIG", errSetattr)
	}

	buf := make([]byte, 100)
	res, errno := handle.Read(ctx, buf, 0)
	if errno != 0 {
		t.Fatalf("Read after failed writes returned errno %v", errno)
	}
	data, status := res.Bytes(buf)
	if !status.Ok() {
		t.Fatalf("Bytes status = %v", status)
	}
	if string(data) != "BASE-OVERLAY" {
		t.Errorf("Read = %q, want %q", string(data), "BASE-OVERLAY")
	}
}

const mappedFileEnv = "WORKSPACE_OVERLAY_MAPPED_FILE"

// TestSharedMappingHelper runs in a child process. A page fault on a mapping
// of a mounted file is answered by the process serving the mount; taking that
// fault inside the serving process itself can deadlock it during a garbage
// collection, so the mapping must live in another process, as it does in use.
func TestSharedMappingHelper(t *testing.T) {
	name := os.Getenv(mappedFileEnv)
	if name == "" {
		return
	}

	f, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	region, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("shared mapping: %v", err)
	}
	region[0] = 'X'
	if err := syscall.Munmap(region); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSharedMappingOfProjectFile(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	_ = mountedProject(t, project)
	dataPath := filepath.Join(project, "data.bin")
	if err := os.WriteFile(dataPath, make([]byte, 8192), 0644); err != nil {
		t.Fatal(err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestSharedMappingHelper$")
	child.Env = append(os.Environ(), mappedFileEnv+"="+dataPath)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("map a mounted project file in another process: %v: %s", err, out)
	}

	data, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 8192 || data[0] != 'X' {
		t.Fatalf("change made through the mapping: %d bytes, first byte %q", len(data), data[0])
	}
}

func TestOpenMergedReturnsEIOWhenFinalContributionUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test requires non-root user")
	}
	n, _, overlayFile := setupMergedNode(t, "BASE-", "OVERLAY")
	if err := os.Chmod(overlayFile, 0); err != nil {
		t.Fatal(err)
	}
	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	h, _, errno := n.openMerged(ctx, syscall.O_RDONLY, parts)
	if h != nil {
		if rel, ok := h.(fs.FileReleaser); ok {
			if errno := rel.Release(ctx); errno != 0 {
				t.Errorf("Release errno = %v", errno)
			}
		}
	}

	if errno != syscall.EIO {
		t.Errorf("openMerged with unreadable final contribution got errno %v, want EIO", errno)
	}
}

func TestSingleContributionInOverlayUsesNativeHandle(t *testing.T) {
	root, _, shared, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(shared, "overlay-only.txt"), "overlay content")
	child := &node{view: root.view, path: "overlay-only.txt"}
	ctx := context.Background()
	h, flags, errno := child.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open returned errno %v", errno)
	}
	if _, ok := h.(*mergedFile); ok {
		t.Fatalf("single contribution from overlay should not be *mergedFile")
	}
	if flags != 0 {
		t.Errorf("flags = %v, want 0", flags)
	}
	if errno := h.(fs.FileReleaser).Release(ctx); errno != 0 {
		t.Fatalf("Release returned errno %v", errno)
	}
}
