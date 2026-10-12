package overlayfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func sniffContribution(t *testing.T, v *View, name string) contribution {
	t.Helper()
	info, err := v.layers[1].root.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	return contribution{index: 1, info: info}
}

func TestSniffReplacementAndPermissions(t *testing.T) {
	for _, change := range []string{"replace", "permissions", "truncate", "fresh bytes"} {
		t.Run(change, func(t *testing.T) {
			v, _, source, _ := setupTestView(t)
			name := "file.txt"
			full := filepath.Join(source, name)
			scratch.Write(t, full, "overlay")
			part := sniffContribution(t, v, name)
			if binary, err := v.binary(name, part); err != nil || binary {
				t.Fatalf("prime: binary=%v error=%v", binary, err)
			}
			wantBinary := false
			var wantErr error
			switch change {
			case "replace":
				scratch.Write(t, full+".new", "\x00binary")
				if err := os.Rename(full+".new", full); err != nil {
					t.Fatal(err)
				}
				part = sniffContribution(t, v, name)
				wantBinary = true
			case "permissions":
				if os.Geteuid() == 0 {
					t.Skip("root can still read a mode-000 file")
				}
				if err := os.Chmod(full, 0); err != nil {
					t.Fatal(err)
				}
				part = sniffContribution(t, v, name)
				wantErr = syscall.EACCES
			case "truncate":
				if err := os.Truncate(full, 0); err != nil {
					t.Fatal(err)
				}
				part = sniffContribution(t, v, name)
			case "fresh bytes":
				scratch.Write(t, full, "\x00binary")
				// Reuse the metadata snapshot to prove no content is cached,
				// even when the source changes between resolution and reading.
				wantBinary = true
			}
			binary, err := v.binary(name, part)
			if !errors.Is(err, wantErr) || binary != wantBinary {
				t.Fatalf("binary=%v error=%v, want binary=%v error=%v", binary, err, wantBinary, wantErr)
			}
		})
	}
}

func TestSniffEvictionAndCleanup(t *testing.T) {
	v, _, source, _ := setupTestView(t)
	read := func(i int) {
		t.Helper()
		name := fmt.Sprintf("file-%d.txt", i)
		scratch.Write(t, filepath.Join(source, name), "text")
		if binary, err := v.binary(name, sniffContribution(t, v, name)); err != nil || binary {
			t.Fatalf("%s: binary=%v error=%v", name, binary, err)
		}
	}
	read(0)
	read(1)
	hot := v.sniffs.files[sniffKey{layer: 1, name: "file-0.txt"}].file
	cold := v.sniffs.files[sniffKey{layer: 1, name: "file-1.txt"}].file
	// Touch the oldest descriptor without changing its source.
	if _, err := v.binary("file-0.txt", sniffContribution(t, v, "file-0.txt")); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= sniffFileLimit; i++ {
		read(i)
	}
	var head [4]byte
	if _, err := cold.ReadAt(head[:], 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("evicted descriptor is still open: %v", err)
	}
	if _, err := hot.ReadAt(head[:], 0); err != nil || string(head[:]) != "text" {
		t.Fatalf("recent descriptor was evicted: %q, %v", head, err)
	}
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	if _, err := hot.ReadAt(head[:], 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("refresh left a descriptor open: %v", err)
	}
	read(0)
	f := v.sniffs.files[sniffKey{layer: 1, name: "file-0.txt"}].file
	v.Close()
	if _, err := f.ReadAt(head[:], 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("view close left a descriptor open: %v", err)
	}
}

func TestSniffReadFailureRecovers(t *testing.T) {
	v, _, source, _ := setupTestView(t)
	name := "file.txt"
	scratch.Write(t, filepath.Join(source, name), "text")
	part := sniffContribution(t, v, name)
	if _, err := v.binary(name, part); err != nil {
		t.Fatal(err)
	}
	f := v.sniffs.files[sniffKey{layer: 1, name: name}].file
	// Simulate a descriptor failure rather than a source-content change.
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := v.binary(name, part); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read failure = %v", err)
	}
	if binary, err := v.binary(name, part); err != nil || binary {
		t.Fatalf("retry: binary=%v error=%v", binary, err)
	}
	if err := os.Remove(filepath.Join(source, name)); err != nil {
		t.Fatal(err)
	}
	v.sniffs.clear()
	if _, err := v.binary(name, part); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source = %v", err)
	}
}

func TestSniffWindow(t *testing.T) {
	v, _, source, _ := setupTestView(t)
	name := "file.txt"
	data := make([]byte, sniffLength+1)
	for i := range data {
		data[i] = 'a'
	}
	data[sniffLength] = 0 // Bytes beyond the sniffed window do not classify it.
	scratch.Write(t, filepath.Join(source, name), string(data))
	if binary, err := v.binary(name, sniffContribution(t, v, name)); err != nil || binary {
		t.Fatalf("binary=%v error=%v", binary, err)
	}
	data[sniffLength-1] = 0
	scratch.Write(t, filepath.Join(source, name), string(data))
	if binary, err := v.binary(name, sniffContribution(t, v, name)); err != nil || !binary {
		t.Fatalf("binary=%v error=%v", binary, err)
	}
	// ReadAt's short-read contract is what the cache normalizes for callers.
	var buf [sniffLength + 2]byte
	if _, err := v.sniffs.files[sniffKey{layer: 1, name: name}].file.ReadAt(buf[:], 0); err != io.EOF {
		t.Fatalf("short read = %v", err)
	}
}

func TestSniffConcurrentEvictionAndInvalidation(t *testing.T) {
	v, _, source, _ := setupTestView(t)
	names := make([]string, sniffFileLimit*2)
	parts := make([]contribution, len(names))
	for i := range names {
		names[i] = fmt.Sprintf("file-%d.txt", i)
		scratch.Write(t, filepath.Join(source, names[i]), "text")
		parts[i] = sniffContribution(t, v, names[i])
	}
	var workers sync.WaitGroup
	for worker := range 16 {
		workers.Go(func() {
			for i := range 200 {
				index := (worker + i) % len(names)
				if binary, err := v.binary(names[index], parts[index]); err != nil || binary {
					t.Errorf("concurrent read: binary=%v error=%v", binary, err)
					return
				}
				if i%20 == 0 {
					v.sniffs.clear()
				}
			}
		})
	}
	workers.Wait()
	v.sniffs.close()
	if _, err := v.binary(names[0], parts[0]); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read after cache close: %v", err)
	}
}
