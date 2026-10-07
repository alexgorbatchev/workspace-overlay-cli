package overlayfs

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

// readCollisionNotice is a helper that opens, reads, and closes a file handle,
// returning its content as a string.
func readCollisionNotice(t *testing.T, child *node, flags uint32) string {
	t.Helper()
	ctx := context.Background()
	h, _, errno := child.Open(ctx, flags)
	if errno != 0 {
		t.Fatalf("Open returned errno %v", errno)
	}

	var result []byte
	for off := int64(0); ; off += 4096 {
		dest := make([]byte, 4096)
		res, errno := h.(fs.FileReader).Read(ctx, dest, off)
		if errno != 0 {
			t.Fatalf("Read returned errno %v", errno)
		}
		data, status := res.Bytes(dest)
		if !status.Ok() {
			t.Fatalf("Bytes status = %v", status)
		}
		if len(data) == 0 {
			break
		}
		result = append(result, data...)
	}

	if rel, ok := h.(fs.FileReleaser); ok {
		if errno := rel.Release(ctx); errno != 0 {
			t.Fatalf("Release errno = %v", errno)
		}
	}
	return string(result)
}

func TestBinaryCollisionServesExplanation(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	binary := "\x00\x01\x02\x03binary"
	scratch.Write(t, filepath.Join(base, "logo.png"), png)
	scratch.Write(t, filepath.Join(shared, "logo.png"), binary)

	child := &node{view: root.view, path: "logo.png"}
	content := readCollisionNotice(t, child, syscall.O_RDONLY)

	if !strings.Contains(content, "logo.png") {
		t.Errorf("content missing file name: %q", content)
	}
	if !strings.Contains(content, "Limitations:") {
		t.Errorf("content missing Limitations section: %q", content)
	}
	if !strings.Contains(content, "To resolve:") {
		t.Errorf("content missing To resolve section: %q", content)
	}
	if strings.Contains(content, "PNG") {
		t.Errorf("content should not contain binary data: %q", content)
	}

	basePath := pathname.Display(filepath.Join(base, "logo.png"))
	sharedPath := pathname.Display(filepath.Join(shared, "logo.png"))
	if !strings.Contains(content, basePath) {
		t.Errorf("content missing base path %q: %q", basePath, content)
	}
	if !strings.Contains(content, sharedPath) {
		t.Errorf("content missing shared path %q: %q", sharedPath, content)
	}
	if !strings.Contains(content, "(project, binary)") {
		t.Errorf("content missing (project, binary) label: %q", content)
	}
	if !strings.Contains(content, "(overlay, binary)") {
		t.Errorf("content missing (overlay, binary) label: %q", content)
	}

	var out fuse.AttrOut
	if errno := child.Getattr(context.Background(), nil, &out); errno != 0 {
		t.Fatalf("Getattr returned errno %v", errno)
	}
	if out.Size != uint64(len(content)) {
		t.Errorf("Getattr size %d, want %d", out.Size, len(content))
	}
}

func TestMixedCollisionIsNotMerged(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	binary := "\x00\x01\x02\x03binary"
	scratch.Write(t, filepath.Join(base, "notes.dat"), "plain notes\n")
	scratch.Write(t, filepath.Join(shared, "notes.dat"), binary)
	child := &node{view: root.view, path: "notes.dat"}
	content := readCollisionNotice(t, child, syscall.O_RDONLY)
	if !strings.Contains(content, "(project, text)") {
		t.Errorf("content missing (project, text) label: %q", content)
	}
	if !strings.Contains(content, "(overlay, binary)") {
		t.Errorf("content missing (overlay, binary) label: %q", content)
	}
	if strings.Contains(content, "plain notes") {
		t.Errorf("content should not contain original file data: %q", content)
	}
}

func TestBinaryCollisionIsReadOnly(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	binary := "\x00\x01\x02\x03binary"
	scratch.Write(t, filepath.Join(base, "logo.png"), png)
	scratch.Write(t, filepath.Join(shared, "logo.png"), binary)
	child := &node{view: root.view, path: "logo.png"}

	ctx := context.Background()
	h, _, errno := child.Open(ctx, syscall.O_RDWR)
	if errno != 0 {
		t.Fatalf("Open for read-write returned errno %v", errno)
	}

	_, writeErrno := h.(fs.FileWriter).Write(ctx, []byte("x"), 0)
	if writeErrno != syscall.EPERM {
		t.Errorf("Write errno %v, want EPERM", writeErrno)
	}

	var in fuse.SetAttrIn
	in.Size = 0
	in.Valid |= fuse.FATTR_SIZE
	var out fuse.AttrOut
	setattrErrno := h.(fs.FileSetattrer).Setattr(ctx, &in, &out)
	if setattrErrno != syscall.EPERM {
		t.Errorf("Setattr errno %v, want EPERM", setattrErrno)
	}

	if rel, ok := h.(fs.FileReleaser); ok {
		if errno := rel.Release(ctx); errno != 0 {
			t.Fatalf("Release returned errno %v", errno)
		}
	}

	basePath := filepath.Join(base, "logo.png")
	sharedPath := filepath.Join(shared, "logo.png")
	if data := scratch.Read(t, basePath); string(data) != png {
		t.Errorf("base file was modified: %q", string(data))
	}
	if data := scratch.Read(t, sharedPath); string(data) != binary {
		t.Errorf("shared file was modified: %q", string(data))
	}
}

func TestBinaryCollisionIsLogged(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	binary := "\x00\x01\x02\x03binary"
	scratch.Write(t, filepath.Join(base, "logo.png"), png)
	scratch.Write(t, filepath.Join(shared, "logo.png"), binary)
	var buffer bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&buffer)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	child := &node{view: root.view, path: "logo.png"}
	readCollisionNotice(t, child, syscall.O_RDONLY)
	logged := buffer.String()
	if !strings.Contains(logged, "cannot merge logo.png") {
		t.Errorf("log missing expected message: %q", logged)
	}
}

func TestTextCollisionStillConcatenates(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(base, "doc.md"), "base\n")
	scratch.Write(t, filepath.Join(shared, "doc.md"), "overlay\n")
	child := &node{view: root.view, path: "doc.md"}
	content := readCollisionNotice(t, child, syscall.O_RDONLY)
	if content != "base\noverlay\n" {
		t.Errorf("concatenation returned %q, want %q", content, "base\noverlay\n")
	}
}

func TestSingleBinaryFileIsServedAsIs(t *testing.T) {
	root, _, shared, _ := setupTestRootNode(t)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	scratch.Write(t, filepath.Join(shared, "solo.png"), png)
	child := &node{view: root.view, path: "solo.png"}
	content := readCollisionNotice(t, child, syscall.O_RDONLY)
	if content != png {
		t.Errorf("single binary file returned %q, want %q", content, png)
	}
}

func TestEditorSaveCannotReplaceBinaryCollision(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	binary := "\x00\x01\x02\x03binary"
	scratch.Write(t, filepath.Join(base, "logo.png"), png)
	scratch.Write(t, filepath.Join(shared, "logo.png"), binary)
	// The staged document keeps the project copy as its prefix, which is what a
	// save of a merged text file looks like; only the collision can reject it.
	staged := png + "replacement"
	scratch.Write(t, filepath.Join(base, "logo.tmp"), staged)

	errno := root.Rename(context.Background(), "logo.tmp", root, "logo.png", 0)
	if errno != syscall.EPERM {
		t.Errorf("Rename errno %v, want EPERM", errno)
	}

	basePath := filepath.Join(base, "logo.png")
	sharedPath := filepath.Join(shared, "logo.png")
	if data := scratch.Read(t, basePath); string(data) != png {
		t.Errorf("base file was modified: %q", string(data))
	}
	if data := scratch.Read(t, sharedPath); string(data) != binary {
		t.Errorf("shared file was modified: %q", string(data))
	}
	tmpPath := filepath.Join(base, "logo.tmp")
	if data := scratch.Read(t, tmpPath); string(data) != staged {
		t.Errorf("tmp file was modified: %q", string(data))
	}
}

func TestMountedBinaryCollisionReadsAsExplanation(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	binary := "\x00\x01\x02\x03binary"
	scratch.Write(t, filepath.Join(project, "logo.png"), png)
	source := filepath.Join(root, "ai")
	scratch.Write(t, filepath.Join(source, "logo.png"), binary)
	_ = mountedProject(t, project, source)

	mountedPath := filepath.Join(project, "logo.png")
	data, err := os.ReadFile(mountedPath)
	if err != nil {
		t.Fatalf("ReadFile through mount: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "To resolve:") {
		t.Errorf("mounted file does not contain explanation: %q", content)
	}

	info, err := os.Stat(mountedPath)
	if err != nil {
		t.Fatalf("Stat through mount: %v", err)
	}
	if info.Size() != int64(len(content)) {
		t.Errorf("Stat size %d, want %d", info.Size(), len(content))
	}

	// A truncating write is what a shell redirect does; it must be refused with a
	// permission error, not an I/O error from a failed handler.
	if err := os.WriteFile(mountedPath, []byte("new"), 0644); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("truncating write to a binary collision: %v, want a permission error", err)
	}
	if err := os.Truncate(mountedPath, 0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("truncate of a binary collision: %v, want a permission error", err)
	}

	sourcePath := filepath.Join(source, "logo.png")
	if sourceData := scratch.Read(t, sourcePath); string(sourceData) != binary {
		t.Errorf("source file was modified: %q", string(sourceData))
	}
}

func TestOversizedBinaryCollisionStillExplains(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	binary := "\x00\x01\x02\x03binary"
	scratch.Write(t, filepath.Join(base, "disk.img"), binary)
	scratch.Write(t, filepath.Join(shared, "disk.img"), binary)
	// Past the 64 MiB merged-size limit: the explanation must win over EFBIG.
	if err := os.Truncate(filepath.Join(base, "disk.img"), 64<<20+1); err != nil {
		t.Fatal(err)
	}
	child := &node{view: root.view, path: "disk.img"}
	if content := readCollisionNotice(t, child, syscall.O_RDONLY); !strings.Contains(content, "To resolve:") {
		t.Fatalf("oversized binary collision: %q", content)
	}
}

func TestEmptyCopyCountsAsText(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(base, "notes.md"), "")
	scratch.Write(t, filepath.Join(shared, "notes.md"), "overlay\n")
	child := &node{view: root.view, path: "notes.md"}
	if got := readCollisionNotice(t, child, syscall.O_RDONLY); got != "overlay\n" {
		t.Fatalf("empty project copy joined with overlay text: %q", got)
	}
}

func TestExplanationReadOffsets(t *testing.T) {
	root, base, shared, _ := setupTestRootNode(t)
	binary := "\x00\x01\x02\x03binary"
	scratch.Write(t, filepath.Join(base, "logo.png"), binary)
	scratch.Write(t, filepath.Join(shared, "logo.png"), binary)
	child := &node{view: root.view, path: "logo.png"}

	whole := readCollisionNotice(t, child, syscall.O_RDONLY)
	ctx := context.Background()
	h, _, errno := child.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	reader := h.(fs.FileReader)

	cases := []struct {
		name   string
		offset int64
		want   string
		errno  syscall.Errno
	}{
		{"middle", 18, whole[18:28], 0},
		{"end", int64(len(whole)), "", 0},
		{"past end", int64(len(whole)) + 100, "", 0},
		{"negative", -1, "", syscall.EINVAL},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dest := make([]byte, 10)
			res, errno := reader.Read(ctx, dest, tt.offset)
			if errno != tt.errno {
				t.Fatalf("Read at %d: errno %v, want %v", tt.offset, errno, tt.errno)
			}
			if errno != 0 {
				return
			}
			data, status := res.Bytes(dest)
			if !status.Ok() || string(data) != tt.want {
				t.Fatalf("Read at %d: %q (%v), want %q", tt.offset, data, status, tt.want)
			}
		})
	}
}
