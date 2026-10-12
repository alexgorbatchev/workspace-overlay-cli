package overlayfs

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMarkerMetadataReadBudget(t *testing.T) {
	root, project, _, _ := setupTestRootNode(t)
	root.view.SetMarkers(DefaultMarkerRules())
	scratch.Write(t, filepath.Join(project, "file.txt"), "project")
	child := &node{view: root.view, path: "file.txt"}
	var out fuse.AttrOut
	allocs := testing.AllocsPerRun(100, func() {
		if errno := child.Getattr(context.Background(), nil, &out); errno != 0 || out.Size != 7 {
			t.Fatalf("Getattr: errno=%v size=%d", errno, out.Size)
		}
	})
	if allocs > 16 {
		t.Errorf("project metadata allocated %.0f times, want <=16", allocs)
	}
}

func TestInodeMetadataReadBudget(t *testing.T) {
	root, project, _, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(project, "file.txt"), "project")
	var entry fuse.EntryOut
	inode, errno := root.Lookup(context.Background(), "file.txt", &entry)
	if errno != 0 {
		t.Fatal(errno)
	}
	root.AddChild("file.txt", inode, true)
	child := inode.Operations().(*node)
	var out fuse.AttrOut
	allocs := testing.AllocsPerRun(100, func() {
		if errno := child.Getattr(context.Background(), nil, &out); errno != 0 || out.Size != 7 {
			t.Fatalf("Getattr: errno=%v size=%d", errno, out.Size)
		}
	})
	if allocs > 19 {
		t.Errorf("inode metadata allocated %.0f times, want <=19", allocs)
	}
}

func TestReaddirReadBudget(t *testing.T) {
	root, project, _, _ := setupTestRootNode(t)
	for i := range 32 {
		scratch.Write(t, filepath.Join(project, fmt.Sprintf("file-%d.txt", i)), "project")
	}
	allocs := testing.AllocsPerRun(100, func() {
		stream, errno := root.Readdir(context.Background())
		if errno != 0 {
			t.Fatal(errno)
		}
		defer stream.Close()
		count := 0
		for stream.HasNext() {
			if _, errno := stream.Next(); errno != 0 {
				t.Fatal(errno)
			}
			count++
		}
		if count != 32 {
			t.Fatalf("listed %d entries, want 32", count)
		}
	})
	if allocs > 300 {
		t.Errorf("listing 32 files allocated %.0f times, want <=300", allocs)
	}
}

func TestMarkedMetadataReadBudget(t *testing.T) {
	root, project, source, _ := setupTestRootNode(t)
	root.view.SetMarkers(DefaultMarkerRules())
	for _, dir := range []string{project, source} {
		scratch.Write(t, filepath.Join(dir, "file.md"), strings.Repeat("a", 64<<10))
	}
	child := &node{view: root.view, path: "file.md"}
	parts, err := root.view.resolve("file.md")
	if err != nil {
		t.Fatal(err)
	}
	want, err := child.renderMarkedData(parts, root.view.findMarkerRule("file.md"))
	if err != nil {
		t.Fatal(err)
	}
	var out fuse.AttrOut
	allocs := testing.AllocsPerRun(100, func() {
		if errno := child.Getattr(context.Background(), nil, &out); errno != 0 || out.Size != uint64(len(want)) {
			t.Fatalf("Getattr: errno=%v size=%d, want %d", errno, out.Size, len(want))
		}
	})
	if allocs > 30 {
		t.Errorf("marked metadata allocated %.0f times, want <=30", allocs)
	}
}
