package overlayfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMarkedMetadataMatchesDocument(t *testing.T) {
	root, project, shared, specific := setupTestRootNode(t)
	root.view.SetMarkers(DefaultMarkerRules())
	values := []string{"", "text", "text\n"}
	for _, base := range values {
		for _, overlay := range values {
			for _, final := range values {
				t.Run(fmt.Sprintf("%q/%q/%q", base, overlay, final), func(t *testing.T) {
					for i, data := range []string{base, overlay, final} {
						scratch.Write(t, filepath.Join([]string{project, shared, specific}[i], "file.md"), data)
					}
					assertMarkedMetadata(t, root.view, "file.md")
				})
			}
		}
	}
	// Keep the size constant while changing the final byte; reusing a
	// descriptor must still read the new byte and adjust the separator.
	for _, data := range []string{"x\n", "xx", "x\n"} {
		scratch.Write(t, filepath.Join(shared, "file.md"), data)
		assertMarkedMetadata(t, root.view, "file.md")
	}
	scratch.Write(t, filepath.Join(shared, "replacement"), "replacement\n")
	if err := os.Rename(filepath.Join(shared, "replacement"), filepath.Join(shared, "file.md")); err != nil {
		t.Fatal(err)
	}
	assertMarkedMetadata(t, root.view, "file.md")
	if err := os.Remove(filepath.Join(project, "file.md")); err != nil {
		t.Fatal(err)
	}
	assertMarkedMetadata(t, root.view, "file.md") // Empty project followed by two overlays.
	if err := os.Remove(filepath.Join(specific, "file.md")); err != nil {
		t.Fatal(err)
	}
	root.view.vacate("file.md", 0644)
	assertMarkedMetadata(t, root.view, "file.md") // Removed project followed by one overlay.
}

func assertMarkedMetadata(t *testing.T, v *View, name string) {
	t.Helper()
	child := &node{view: v, path: name}
	parts, err := v.resolve(name)
	if err != nil {
		t.Fatal(err)
	}
	rule := v.findMarkerRule(name)
	parts = v.document(name, parts)
	data, err := child.renderMarkedData(parts, rule)
	if err != nil {
		t.Fatal(err)
	}
	var out fuse.AttrOut
	if errno := child.Getattr(context.Background(), nil, &out); errno != 0 || out.Size != uint64(len(data)) {
		t.Fatalf("Getattr: errno=%v size=%d, rendered size=%d", errno, out.Size, len(data))
	}
}
