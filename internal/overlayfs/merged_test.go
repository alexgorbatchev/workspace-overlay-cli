package overlayfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestContributionBytes(t *testing.T) {
	tests := []struct {
		name      string
		data      []byte
		prefix    []byte
		want      []byte
		wantErr   bool
		errTarget error
	}{
		{
			name:      "empty data",
			data:      nil,
			prefix:    []byte("prefix"),
			want:      nil,
			wantErr:   false,
			errTarget: nil,
		},
		{
			name:      "zero length data",
			data:      []byte{},
			prefix:    []byte("prefix"),
			want:      nil,
			wantErr:   false,
			errTarget: nil,
		},
		{
			name:      "valid prefix match",
			data:      []byte("prefix-suffix"),
			prefix:    []byte("prefix-"),
			want:      []byte("suffix"),
			wantErr:   false,
			errTarget: nil,
		},
		{
			name:      "exact prefix match with empty suffix",
			data:      []byte("prefix"),
			prefix:    []byte("prefix"),
			want:      []byte(""),
			wantErr:   false,
			errTarget: nil,
		},
		{
			name:      "prefix mismatch",
			data:      []byte("corrupted-prefix-suffix"),
			prefix:    []byte("expected-prefix-"),
			want:      nil,
			wantErr:   true,
			errTarget: syscall.EPERM,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := contributionBytes(tt.data, tt.prefix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("contributionBytes error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.errTarget != nil && !errors.Is(err, tt.errTarget) {
					t.Errorf("got error %v, want %v", err, tt.errTarget)
				}
			} else {
				if string(got) != string(tt.want) {
					t.Errorf("got %q, want %q", string(got), string(tt.want))
				}
			}
		})
	}
}

func setupMergedNode(t *testing.T, baseContent, overlayContent string) (*node, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "base")
	overlayDir := filepath.Join(tmpDir, "overlay")

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatal(err)
	}

	baseFile := filepath.Join(baseDir, "test.txt")
	overlayFile := filepath.Join(overlayDir, "test.txt")

	if err := os.WriteFile(baseFile, []byte(baseContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlayFile, []byte(overlayContent), 0644); err != nil {
		t.Fatal(err)
	}

	baseRoot, err := os.OpenRoot(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	overlayRoot, err := os.OpenRoot(overlayDir)
	if err != nil {
		t.Fatal(err)
	}

	v := &View{
		layers: []layer{
			{root: baseRoot},
			{root: overlayRoot},
		},
	}
	t.Cleanup(func() { v.Close() })

	n := &node{view: v, path: "test.txt"}
	return n, baseFile, overlayFile
}

func TestMergedFileRead(t *testing.T) {
	n, _, _ := setupMergedNode(t, "BASE-", "OVERLAY")
	ctx := context.Background()

	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Read normal
	h, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
	if errno != 0 {
		t.Fatalf("openMerged errno = %v", errno)
	}
	handle := h.(*mergedFile)
	defer func() {
		if errno := handle.Release(ctx); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	buf := make([]byte, 100)
	res, errno := handle.Read(ctx, buf, 0)
	if errno != 0 {
		t.Fatalf("Read errno = %v", errno)
	}
	data, status := res.Bytes(buf)
	if !status.Ok() {
		t.Fatalf("Bytes status = %v", status)
	}
	if string(data) != "BASE-OVERLAY" {
		t.Errorf("Read got %q, want %q", string(data), "BASE-OVERLAY")
	}

	// 2. Read with offset
	res, errno = handle.Read(ctx, buf, 5)
	if errno != 0 {
		t.Fatalf("Read with offset errno = %v", errno)
	}
	data, status = res.Bytes(buf)
	if !status.Ok() {
		t.Fatalf("Bytes status = %v", status)
	}
	if string(data) != "OVERLAY" {
		t.Errorf("Read at offset 5 got %q, want %q", string(data), "OVERLAY")
	}

	// 3. Read beyond EOF
	res, errno = handle.Read(ctx, buf, 100)
	if errno != 0 {
		t.Fatalf("Read beyond EOF errno = %v", errno)
	}
	data, status = res.Bytes(buf)
	if !status.Ok() {
		t.Fatalf("Bytes status = %v", status)
	}
	if len(data) != 0 {
		t.Errorf("Read beyond EOF got %q, want empty", string(data))
	}

	// 4. Read with negative offset
	_, errno = handle.Read(ctx, buf, -1)
	if errno != syscall.EINVAL {
		t.Errorf("Read with negative offset got errno %v, want EINVAL", errno)
	}

	// 5. Read on write-only handle
	woH, _, errno := n.openMerged(ctx, syscall.O_WRONLY, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	woHandle := woH.(*mergedFile)
	defer func() {
		if errno := woHandle.Release(ctx); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()
	_, errno = woHandle.Read(ctx, buf, 0)
	if errno != syscall.EBADF {
		t.Errorf("Read on O_WRONLY got errno %v, want EBADF", errno)
	}
}

func TestMergedFileWriteValid(t *testing.T) {
	n, baseFile, overlayFile := setupMergedNode(t, "BASE-", "OLD_OVERLAY")
	ctx := context.Background()

	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	h, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	handle := h.(*mergedFile)

	// Overwrite starting at suffix offset 5
	newSuffix := []byte("NEW_OVERLAY")
	written, errno := handle.Write(ctx, newSuffix, 5)
	if errno != 0 || int(written) != len(newSuffix) {
		t.Fatalf("Write errno = %v, written = %d", errno, written)
	}

	// Commit via Fsync
	if errno := handle.Fsync(ctx, 0); errno != 0 {
		t.Fatalf("Fsync errno = %v", errno)
	}

	// Check underlying overlay file: must have NEW_OVERLAY, NOT the prefix!
	data, err := os.ReadFile(overlayFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "NEW_OVERLAY" {
		t.Errorf("underlying overlay file got %q, want %q", string(data), "NEW_OVERLAY")
	}

	// Check underlying base file: must be unchanged
	baseData, err := os.ReadFile(baseFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(baseData) != "BASE-" {
		t.Errorf("underlying base file got %q, want %q", string(baseData), "BASE-")
	}

	// Release handle
	if errno := handle.Release(ctx); errno != 0 {
		t.Errorf("Release errno = %v", errno)
	}
}

func TestMergedFileWritePrefixViolation(t *testing.T) {
	n, _, _ := setupMergedNode(t, "BASE-", "OVERLAY")
	ctx := context.Background()

	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	h, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	handle := h.(*mergedFile)
	defer func() {
		// Release may return EPERM if the handle is in a failed state; the test intentionally put it there
		_ = handle.Release(ctx)
	}()

	// Overwrite at offset 0 modifying the prefix
	_, errno = handle.Write(ctx, []byte("FAIL-"), 0)
	if errno != syscall.EPERM {
		t.Errorf("Write modifying prefix got errno %v, want EPERM", errno)
	}

	// Flush/commit after failure should return EPERM
	if errno := handle.Flush(ctx); errno != syscall.EPERM {
		t.Errorf("Flush after failed write got errno %v, want EPERM", errno)
	}
}

func TestMergedFileAppendMode(t *testing.T) {
	n, _, overlayFile := setupMergedNode(t, "BASE-", "OVERLAY")
	ctx := context.Background()

	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	h, _, errno := n.openMerged(ctx, syscall.O_RDWR|syscall.O_APPEND, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	handle := h.(*mergedFile)

	written, errno := handle.Write(ctx, []byte("-EXTRA"), 0)
	if errno != 0 || int(written) != len("-EXTRA") {
		t.Fatalf("Write append errno = %v, written = %d", errno, written)
	}

	if errno := handle.Flush(ctx); errno != 0 {
		t.Fatalf("Flush errno = %v", errno)
	}
	if errno := handle.Release(ctx); errno != 0 {
		t.Errorf("Release errno = %v", errno)
	}

	data, err := os.ReadFile(overlayFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "OVERLAY-EXTRA" {
		t.Errorf("append result in overlay file = %q, want %q", string(data), "OVERLAY-EXTRA")
	}
}

func TestMergedFileTruncateAndSetattr(t *testing.T) {
	n, _, overlayFile := setupMergedNode(t, "BASE-", "OVERLAY")
	ctx := context.Background()

	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	h, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	handle := h.(*mergedFile)

	// Truncate to size 7 ("BASE-OV")
	var setIn fuse.SetAttrIn
	setIn.Size = 7
	setIn.Valid |= fuse.FATTR_SIZE
	var out fuse.AttrOut
	if errno := handle.Setattr(ctx, &setIn, &out); errno != 0 {
		t.Fatalf("Setattr size errno = %v", errno)
	}
	if out.Size != 7 {
		t.Errorf("Getattr size after truncate = %d, want 7", out.Size)
	}

	// Check Getattr
	var getOut fuse.AttrOut
	if errno := handle.Getattr(ctx, &getOut); errno != 0 {
		t.Fatalf("Getattr errno = %v", errno)
	}
	if getOut.Size != 7 {
		t.Errorf("Getattr size = %d, want 7", getOut.Size)
	}

	// Commit and verify overlay file
	if errno := handle.Flush(ctx); errno != 0 {
		t.Fatalf("Flush errno = %v", errno)
	}
	if errno := handle.Release(ctx); errno != 0 {
		t.Errorf("Release errno = %v", errno)
	}

	data, err := os.ReadFile(overlayFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "OV" {
		t.Errorf("overlay after truncate = %q, want %q", string(data), "OV")
	}
}

func TestMergedFileReadOnlyWrite(t *testing.T) {
	n, _, _ := setupMergedNode(t, "BASE-", "OVERLAY")
	ctx := context.Background()

	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	h, _, errno := n.openMerged(ctx, syscall.O_RDONLY, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	handle := h.(*mergedFile)
	defer func() {
		if errno := handle.Release(ctx); errno != 0 {
			t.Errorf("Release errno = %v", errno)
		}
	}()

	_, errno = handle.Write(ctx, []byte("MORE"), 5)
	if errno != syscall.EBADF {
		t.Errorf("Write on read-only handle got errno %v, want EBADF", errno)
	}

	var in fuse.SetAttrIn
	in.Size = 10
	in.Valid |= fuse.FATTR_SIZE
	var out fuse.AttrOut
	if errno := handle.Setattr(ctx, &in, &out); errno != syscall.EBADF {
		t.Errorf("Setattr on read-only handle got errno %v, want EBADF", errno)
	}
}

func TestMergedFileTruncOnOpenAndExtend(t *testing.T) {
	n, _, _ := setupMergedNode(t, "BASE-", "OVERLAY")
	ctx := context.Background()

	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Open with O_TRUNC clears data and sets dirty
	h, _, errno := n.openMerged(ctx, syscall.O_RDWR|syscall.O_TRUNC, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	handle := h.(*mergedFile)
	if len(handle.data) != 0 || !handle.dirty {
		t.Errorf("O_TRUNC should clear data and set dirty, got len=%d, dirty=%v", len(handle.data), handle.dirty)
	}
	if errno := handle.Release(ctx); errno != 0 {
		t.Errorf("Release errno = %v", errno)
	}

	// 2. Commit when not dirty returns 0
	h2, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	handle2 := h2.(*mergedFile)
	handle2.dirty = false
	if errno := handle2.commit(); errno != 0 {
		t.Errorf("commit when not dirty got errno %v, want 0", errno)
	}

	// 3. Setattr extending file size
	var in fuse.SetAttrIn
	in.Size = 25
	in.Valid |= fuse.FATTR_SIZE
	var out fuse.AttrOut
	if errno := handle2.Setattr(ctx, &in, &out); errno != 0 {
		t.Fatalf("Setattr extend size errno = %v", errno)
	}
	if out.Size != 25 || len(handle2.data) != 25 {
		t.Errorf("Setattr extend got size = %d, len = %d, want 25", out.Size, len(handle2.data))
	}

	// 4. Setattr size too large (EFBIG)
	in.Size = uint64(int(^uint(0)>>1)) + 100
	if errno := handle2.Setattr(ctx, &in, &out); errno != syscall.EFBIG {
		t.Errorf("Setattr huge size got errno %v, want EFBIG", errno)
	}

	// 5. Fsync after failure returns error
	handle2.failed = syscall.EPERM
	if errno := handle2.Fsync(ctx, 0); errno != syscall.EPERM {
		t.Errorf("Fsync after failure got errno %v, want EPERM", errno)
	}

	// 6. Release after failure returns error
	if errno := handle2.Release(ctx); errno != syscall.EPERM {
		t.Errorf("Release after failure got errno %v, want EPERM", errno)
	}

	// 8. Write with out of range offset (EFBIG)
	h4, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
	if errno != 0 {
		t.Fatal(errno)
	}
	handle4 := h4.(*mergedFile)
	hugeOff := int64(int(^uint(0) >> 1))
	_, errno = handle4.Write(ctx, []byte("data"), hugeOff)
	if errno != syscall.EFBIG {
		t.Errorf("Write with huge offset got errno %v, want EFBIG", errno)
	}
	if errno := handle4.Release(ctx); errno != 0 {
		t.Errorf("Release errno = %v", errno)
	}
}

func TestOpenMergedErrors(t *testing.T) {
	n, _, _ := setupMergedNode(t, "BASE-", "OVERLAY")
	ctx := context.Background()

	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	// Close last layer root so OpenFile fails
	if err := n.view.layers[parts[len(parts)-1].index].root.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
	if errno == 0 {
		t.Errorf("openMerged on closed layer root should fail")
	}
}
