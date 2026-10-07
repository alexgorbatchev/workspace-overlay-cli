package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
)

func setupTestView(t *testing.T) (*view, string, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "base")
	sharedDir := filepath.Join(tmpDir, "shared")
	specDir := filepath.Join(tmpDir, "specific")

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(specDir, 0755); err != nil {
		t.Fatal(err)
	}

	baseRoot, err := os.OpenRoot(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	sharedRoot, err := os.OpenRoot(sharedDir)
	if err != nil {
		t.Fatal(err)
	}
	specRoot, err := os.OpenRoot(specDir)
	if err != nil {
		t.Fatal(err)
	}

	v := &view{
		layers: []layer{
			{root: baseRoot},
			{root: sharedRoot},
			{root: specRoot},
		},
	}
	t.Cleanup(func() { v.close() })
	return v, baseDir, sharedDir, specDir
}

func TestResolve(t *testing.T) {
	v, baseDir, sharedDir, specDir := setupTestView(t)

	// File in all 3 layers
	_ = os.WriteFile(filepath.Join(baseDir, "all.txt"), []byte("0"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "all.txt"), []byte("1"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "all.txt"), []byte("2"), 0644)

	parts, err := v.resolve("all.txt")
	if err != nil {
		t.Fatalf("resolve all.txt error = %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}
	for i, part := range parts {
		if part.index != i {
			t.Errorf("part %d has index %d, want %d", i, part.index, i)
		}
	}

	// File in layer 0 only
	_ = os.WriteFile(filepath.Join(baseDir, "base_only.txt"), []byte("b"), 0644)
	parts, err = v.resolve("base_only.txt")
	if err != nil || len(parts) != 1 || parts[0].index != 0 {
		t.Fatalf("resolve base_only.txt: got %v, err %v", parts, err)
	}

	// File in layer 1 and 2 only
	_ = os.WriteFile(filepath.Join(sharedDir, "overlay.txt"), []byte("s"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "overlay.txt"), []byte("p"), 0644)
	parts, err = v.resolve("overlay.txt")
	if err != nil || len(parts) != 2 || parts[0].index != 1 || parts[1].index != 2 {
		t.Fatalf("resolve overlay.txt: got %v, err %v", parts, err)
	}

	// File in layer 2 only
	_ = os.WriteFile(filepath.Join(specDir, "spec_only.txt"), []byte("p"), 0644)
	parts, err = v.resolve("spec_only.txt")
	if err != nil || len(parts) != 1 || parts[0].index != 2 {
		t.Fatalf("resolve spec_only.txt: got %v, err %v", parts, err)
	}

	// Non-existent file
	parts, err = v.resolve("missing.txt")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolve missing.txt expected ErrNotExist, got %v", err)
	}

	// Incompatible types: directory in layer 0, regular file in layer 1
	_ = os.Mkdir(filepath.Join(baseDir, "mismatch"), 0755)
	_ = os.WriteFile(filepath.Join(sharedDir, "mismatch"), []byte("not a dir"), 0644)
	_, err = v.resolve("mismatch")
	if err == nil || !errors.Is(err, syscall.EIO) {
		t.Fatalf("resolve mismatch expected EIO error, got %v", err)
	}
}

func TestContents(t *testing.T) {
	v, baseDir, sharedDir, specDir := setupTestView(t)

	_ = os.WriteFile(filepath.Join(baseDir, "doc.txt"), []byte("prefix-"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "doc.txt"), []byte("shared-"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "doc.txt"), []byte("suffix"), 0644)

	parts, err := v.resolve("doc.txt")
	if err != nil {
		t.Fatal(err)
	}

	data, err := v.contents("doc.txt", parts)
	if err != nil {
		t.Fatalf("contents doc.txt: %v", err)
	}
	want := "prefix-shared-suffix"
	if string(data) != want {
		t.Errorf("contents = %q, want %q", string(data), want)
	}
}

func TestEntries(t *testing.T) {
	v, baseDir, sharedDir, specDir := setupTestView(t)

	_ = os.MkdirAll(filepath.Join(baseDir, "sub"), 0755)
	_ = os.MkdirAll(filepath.Join(sharedDir, "sub"), 0755)
	_ = os.MkdirAll(filepath.Join(specDir, "sub"), 0755)

	_ = os.WriteFile(filepath.Join(baseDir, "sub", "f0.txt"), []byte("0"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "sub", "f1.txt"), []byte("1"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "sub", "f2.txt"), []byte("2"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "sub", "common.txt"), []byte("c1"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "sub", "common.txt"), []byte("c2"), 0644)

	entries, err := v.entries("sub")
	if err != nil {
		t.Fatalf("entries sub: %v", err)
	}

	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	want := []string{"common.txt", "f0.txt", "f1.txt", "f2.txt"}
	if len(names) != len(want) {
		t.Fatalf("entries count = %d, want %d: %v", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("entry[%d] = %q, want %q", i, names[i], want[i])
		}
	}

	// Calling entries on a regular file returns ENOTDIR
	_, err = v.entries("sub/f0.txt")
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("entries on regular file expected ENOTDIR, got %v", err)
	}
}

func TestDestination(t *testing.T) {
	v, baseDir, sharedDir, specDir := setupTestView(t)

	// Existing files: returns most-specific contributing layer
	_ = os.WriteFile(filepath.Join(baseDir, "file0.txt"), []byte("0"), 0644)
	_ = os.WriteFile(filepath.Join(sharedDir, "file1.txt"), []byte("1"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "file2.txt"), []byte("2"), 0644)
	_ = os.WriteFile(filepath.Join(baseDir, "shared_both.txt"), []byte("0"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "shared_both.txt"), []byte("2"), 0644)

	dest, err := v.destination("file0.txt")
	if err != nil || dest != 0 {
		t.Errorf("destination file0.txt = %d, err %v, want 0", dest, err)
	}
	dest, err = v.destination("file1.txt")
	if err != nil || dest != 1 {
		t.Errorf("destination file1.txt = %d, err %v, want 1", dest, err)
	}
	dest, err = v.destination("file2.txt")
	if err != nil || dest != 2 {
		t.Errorf("destination file2.txt = %d, err %v, want 2", dest, err)
	}
	dest, err = v.destination("shared_both.txt")
	if err != nil || dest != 2 {
		t.Errorf("destination shared_both.txt = %d, err %v, want 2", dest, err)
	}

	// New files in existing base directory: returns layer 0
	_ = os.MkdirAll(filepath.Join(baseDir, "project_dir"), 0755)
	dest, err = v.destination("project_dir/new_file.txt")
	if err != nil || dest != 0 {
		t.Errorf("destination new file in project_dir = %d, err %v, want 0", dest, err)
	}

	// New files in overlay-only directory: returns most-specific overlay layer
	_ = os.MkdirAll(filepath.Join(sharedDir, "overlay_dir"), 0755)
	_ = os.MkdirAll(filepath.Join(specDir, "overlay_dir"), 0755)
	dest, err = v.destination("overlay_dir/new_file.txt")
	if err != nil || dest != 2 {
		t.Errorf("destination new file in overlay_dir = %d, err %v, want 2", dest, err)
	}

	// Parent path is not a directory
	dest, err = v.destination("file0.txt/invalid_child.txt")
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("expected ENOTDIR when parent is file, got %v", err)
	}
}

func TestStable(t *testing.T) {
	v, baseDir, _, specDir := setupTestView(t)

	_ = os.WriteFile(filepath.Join(baseDir, "f1.txt"), []byte("1"), 0644)
	_ = os.WriteFile(filepath.Join(specDir, "f2.txt"), []byte("2"), 0644)

	attr1, err := v.stable("f1.txt")
	if err != nil {
		t.Fatalf("stable f1.txt: %v", err)
	}
	if attr1.Ino < 2 {
		t.Errorf("expected Ino >= 2, got %d", attr1.Ino)
	}

	// Second call for f1.txt returns the exact same inode
	attr1Again, err := v.stable("f1.txt")
	if err != nil {
		t.Fatalf("stable f1.txt second call: %v", err)
	}
	if attr1Again.Ino != attr1.Ino {
		t.Errorf("stable Ino mismatch: %d != %d", attr1Again.Ino, attr1.Ino)
	}

	// Different file f2.txt gets a different inode
	attr2, err := v.stable("f2.txt")
	if err != nil {
		t.Fatalf("stable f2.txt: %v", err)
	}
	if attr2.Ino == attr1.Ino {
		t.Errorf("different files received identical Ino %d", attr2.Ino)
	}

	// Missing file returns error
	_, err = v.stable("nonexistent.txt")
	if err == nil {
		t.Errorf("expected error for missing file, got nil")
	}
}

func TestViewCloseTwice(t *testing.T) {
	v, _, _, _ := setupTestView(t)
	v.close()
	// Close again generates error on layer.root.Close()
	v.close()
}

func TestViewContentsError(t *testing.T) {
	v, baseDir, _, _ := setupTestView(t)
	_ = os.WriteFile(filepath.Join(baseDir, "f.txt"), []byte("data"), 0644)
	parts, err := v.resolve("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = v.layers[0].root.Close()
	_, err = v.contents("f.txt", parts)
	if err == nil {
		t.Errorf("contents on closed layer root should fail")
	}
}
