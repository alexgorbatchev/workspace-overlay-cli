package overlayfs

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func setupTestView(t *testing.T) (*View, string, string, string) {
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

	// Overlay layers without a glob include everything, so tests can add files
	// without re-reading the path index.
	v := &View{layers: []layer{{root: baseRoot}, {root: sharedRoot}, {root: specRoot}}}
	t.Cleanup(func() { v.Close() })
	return v, baseDir, sharedDir, specDir
}

// mountedProject serves project merged with the given overlay source
// directories for the duration of the test.
func mountedProject(t *testing.T, project string, sources ...string) *View {
	t.Helper()
	projectRoot, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	v := &View{}
	v.AddProject(projectRoot)
	for _, source := range sources {
		sourceRoot, err := os.OpenRoot(source)
		if err != nil {
			t.Fatal(err)
		}
		v.AddOverlay(sourceRoot, filepath.Base(source), "**/*")
	}
	if err := v.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	server, err := v.Mount(project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		v.Close()
	})
	// Tests read the mount from the process that serves it, which is what
	// WaitMount prepares a mount for and View.Mount leaves out.
	if err := server.WaitMount(); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestResolve(t *testing.T) {
	v, baseDir, sharedDir, specDir := setupTestView(t)

	// File in all 3 layers
	scratch.Write(t, filepath.Join(baseDir, "all.txt"), "0")
	scratch.Write(t, filepath.Join(sharedDir, "all.txt"), "1")
	scratch.Write(t, filepath.Join(specDir, "all.txt"), "2")

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
	scratch.Write(t, filepath.Join(baseDir, "base_only.txt"), "b")
	parts, err = v.resolve("base_only.txt")
	if err != nil || len(parts) != 1 || parts[0].index != 0 {
		t.Fatalf("resolve base_only.txt: got %v, err %v", parts, err)
	}

	// File in layer 1 and 2 only
	scratch.Write(t, filepath.Join(sharedDir, "overlay.txt"), "s")
	scratch.Write(t, filepath.Join(specDir, "overlay.txt"), "p")
	parts, err = v.resolve("overlay.txt")
	if err != nil || len(parts) != 2 || parts[0].index != 1 || parts[1].index != 2 {
		t.Fatalf("resolve overlay.txt: got %v, err %v", parts, err)
	}

	// File in layer 2 only
	scratch.Write(t, filepath.Join(specDir, "spec_only.txt"), "p")
	parts, err = v.resolve("spec_only.txt")
	if err != nil || len(parts) != 1 || parts[0].index != 2 {
		t.Fatalf("resolve spec_only.txt: got %v, err %v", parts, err)
	}

	// Non-existent file
	_, err = v.resolve("missing.txt")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolve missing.txt expected ErrNotExist, got %v", err)
	}

	// Incompatible types: directory in layer 0, regular file in layer 1
	scratch.Mkdir(t, filepath.Join(baseDir, "mismatch"))
	scratch.Write(t, filepath.Join(sharedDir, "mismatch"), "not a dir")
	_, err = v.resolve("mismatch")
	if err == nil || !errors.Is(err, syscall.EIO) {
		t.Fatalf("resolve mismatch expected EIO error, got %v", err)
	}
}

func TestContents(t *testing.T) {
	v, baseDir, sharedDir, specDir := setupTestView(t)

	scratch.Write(t, filepath.Join(baseDir, "doc.txt"), "prefix-")
	scratch.Write(t, filepath.Join(sharedDir, "doc.txt"), "shared-")
	scratch.Write(t, filepath.Join(specDir, "doc.txt"), "suffix")

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

	scratch.Mkdir(t, filepath.Join(baseDir, "sub"))
	scratch.Mkdir(t, filepath.Join(sharedDir, "sub"))
	scratch.Mkdir(t, filepath.Join(specDir, "sub"))

	scratch.Write(t, filepath.Join(baseDir, "sub", "f0.txt"), "0")
	scratch.Write(t, filepath.Join(sharedDir, "sub", "f1.txt"), "1")
	scratch.Write(t, filepath.Join(specDir, "sub", "f2.txt"), "2")
	scratch.Write(t, filepath.Join(sharedDir, "sub", "common.txt"), "c1")
	scratch.Write(t, filepath.Join(specDir, "sub", "common.txt"), "c2")

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
	scratch.Write(t, filepath.Join(baseDir, "file0.txt"), "0")
	scratch.Write(t, filepath.Join(sharedDir, "file1.txt"), "1")
	scratch.Write(t, filepath.Join(specDir, "file2.txt"), "2")
	scratch.Write(t, filepath.Join(baseDir, "shared_both.txt"), "0")
	scratch.Write(t, filepath.Join(specDir, "shared_both.txt"), "2")

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
	scratch.Mkdir(t, filepath.Join(baseDir, "project_dir"))
	dest, err = v.destination("project_dir/new_file.txt")
	if err != nil || dest != 0 {
		t.Errorf("destination new file in project_dir = %d, err %v, want 0", dest, err)
	}

	// New files in overlay-only directory: returns most-specific overlay layer
	scratch.Mkdir(t, filepath.Join(sharedDir, "overlay_dir"))
	scratch.Mkdir(t, filepath.Join(specDir, "overlay_dir"))
	dest, err = v.destination("overlay_dir/new_file.txt")
	if err != nil || dest != 2 {
		t.Errorf("destination new file in overlay_dir = %d, err %v, want 2", dest, err)
	}

	// Parent path is not a directory
	_, err = v.destination("file0.txt/invalid_child.txt")
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("expected ENOTDIR when parent is file, got %v", err)
	}
}

func TestStable(t *testing.T) {
	v, baseDir, _, specDir := setupTestView(t)

	scratch.Write(t, filepath.Join(baseDir, "f1.txt"), "1")
	scratch.Write(t, filepath.Join(specDir, "f2.txt"), "2")

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

	var buffer bytes.Buffer
	log.SetOutput(&buffer)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	v.Close()
	firstLog := buffer.String()
	if firstLog != "" {
		t.Errorf("first close logged unexpectedly: %s", firstLog)
	}

	// Reset buffer for second close
	buffer.Reset()
	v.Close()
	// Note: second close may or may not log depending on whether os.Root.Close()
	// returns an error when called on an already-closed handle. Verify it doesn't panic.
}

func TestViewContentsError(t *testing.T) {
	v, baseDir, _, _ := setupTestView(t)
	scratch.Write(t, filepath.Join(baseDir, "f.txt"), "data")
	parts, err := v.resolve("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.layers[0].root.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = v.contents("f.txt", parts)
	if err == nil {
		t.Errorf("contents on closed layer root should fail")
	}
}
