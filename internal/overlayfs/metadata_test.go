package overlayfs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestCallerClassificationOnlyWhenNeeded(t *testing.T) {
	root, project, source, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(project, "plain.txt"), "project")
	scratch.Write(t, filepath.Join(project, "merged.txt"), "project")
	scratch.Write(t, filepath.Join(source, "merged.txt"), "overlay")
	original := readProcessComm
	t.Cleanup(func() { readProcessComm = original })
	calls := 0
	readProcessComm = func(uint32) string { calls++; return "editor" }
	ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 123})
	for _, test := range []struct {
		name string
		want int
	}{{"plain.txt", 0}, {"merged.txt", 1}} {
		t.Run(test.name, func(t *testing.T) {
			child := &node{view: root.view, path: test.name}
			calls = 0
			var out fuse.AttrOut
			if errno := child.Getattr(ctx, nil, &out); errno != 0 {
				t.Fatal(errno)
			}
			if calls != test.want {
				t.Errorf("Getattr classified caller %d times, want %d", calls, test.want)
			}
			calls = 0
			handle, _, errno := child.Open(ctx, syscall.O_RDONLY)
			if errno != 0 {
				t.Fatal(errno)
			}
			if errno := handle.(fs.FileReleaser).Release(ctx); errno != 0 {
				t.Fatal(errno)
			}
			if calls != test.want {
				t.Errorf("Open classified caller %d times, want %d", calls, test.want)
			}
		})
	}
}

func TestLookupUsesOneLayerSnapshot(t *testing.T) {
	root, project, source, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(project, "file.txt"), "project")
	scratch.Write(t, filepath.Join(source, "file.txt"), "overlay")
	original := readProcessComm
	t.Cleanup(func() { readProcessComm = original })
	readProcessComm = func(uint32) string {
		// Model removal between metadata calculation and inode publication.
		for _, dir := range []string{project, source} {
			if err := os.Remove(filepath.Join(dir, "file.txt")); err != nil {
				t.Fatal(err)
			}
		}
		return "editor"
	}
	ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 123})
	var out fuse.EntryOut
	if inode, errno := root.Lookup(ctx, "file.txt", &out); errno != 0 || inode == nil {
		t.Fatalf("Lookup discarded its resolved snapshot: inode=%v errno=%v", inode, errno)
	}
}

func BenchmarkMetadata(b *testing.B) {
	for _, merged := range []bool{false, true} {
		name := "project"
		if merged {
			name = "merged"
		}
		b.Run(name, func(b *testing.B) {
			project := b.TempDir()
			source := b.TempDir()
			for _, dir := range []string{project, source} {
				if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("contents"), 0644); err != nil {
					b.Fatal(err)
				}
			}
			var v View
			for i, dir := range []string{project, source} {
				r, err := os.OpenRoot(dir)
				if err != nil {
					b.Fatal(err)
				}
				if i == 0 {
					v.AddProject(r)
				} else if merged {
					v.AddOverlay(r, "source", "**/*")
				} else if err := r.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.Cleanup(v.Close)
			if err := v.RefreshPaths(); err != nil {
				b.Fatal(err)
			}
			root := &node{view: &v, path: "."}
			fs.NewNodeFS(root, nil)
			ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: uint32(os.Getpid())})
			b.ResetTimer()
			for b.Loop() {
				var out fuse.EntryOut
				if _, errno := root.Lookup(ctx, "file.txt", &out); errno != 0 {
					b.Fatal(errno)
				}
			}
		})
	}
}
