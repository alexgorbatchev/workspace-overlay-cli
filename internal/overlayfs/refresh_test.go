package overlayfs

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitexclude"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func setupTestRefreshNode(t *testing.T, withExclude bool) (*node, string, string) {
	t.Helper()
	root, base, shared, _ := setupTestRootNode(t)
	scratch.Write(t, filepath.Join(shared, "docs/guide.md"), "guide")
	root.view.layers[1].rules = &pathRules{glob: "**/*"}
	root.view.layers[2].rules = &pathRules{glob: "**/*"}
	if err := root.view.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	if withExclude {
		scratch.GitRepo(t, base)
		g, err := gitexclude.Open(context.Background(), base)
		if err != nil {
			t.Fatal(err)
		}
		root.view.exclude = g
		t.Cleanup(func() {
			if err := g.Close(); err != nil {
				t.Log(err)
			}
		})
	}
	return root, base, shared
}

func TestProjectMutationDoesNotReindexOverlays(t *testing.T) {
	root, _, _ := setupTestRefreshNode(t, false)
	ctx := context.Background()
	root.view.layers[1].rules.glob = "["
	var out fuse.EntryOut
	_, _, _, errno := root.Create(ctx, "project.txt", syscall.O_RDWR, 0644, &out)
	if errno != 0 {
		t.Fatalf("Create project.txt: got errno %v, want 0", errno)
	}

	_, errno = root.Mkdir(ctx, "builddir", 0755, &out)
	if errno != 0 {
		t.Fatalf("Mkdir builddir: got errno %v, want 0", errno)
	}

	errno = root.Unlink(ctx, "project.txt")
	if errno != 0 {
		t.Fatalf("Unlink project.txt: got errno %v, want 0", errno)
	}

	errno = root.Rmdir(ctx, "builddir")
	if errno != 0 {
		t.Fatalf("Rmdir builddir: got errno %v, want 0", errno)
	}
}

func TestOverlayMutationStillReindexes(t *testing.T) {
	root, _, _ := setupTestRefreshNode(t, false)
	ctx := context.Background()
	dir := &node{view: root.view, path: "docs"}
	fs.NewNodeFS(dir, nil)
	var out fuse.EntryOut
	_, _, _, errno := dir.Create(ctx, "new.md", syscall.O_RDWR, 0644, &out)
	if errno != 0 {
		t.Fatalf("Create new.md: got errno %v, want 0", errno)
	}

	_, err := root.view.resolve("docs/new.md")
	if err != nil {
		t.Fatalf("resolve docs/new.md: %v", err)
	}
}

func TestExclusionsAreNotRecomputedWhenNothingChanged(t *testing.T) {
	root, _, _ := setupTestRefreshNode(t, true)
	ctx := context.Background()
	exclude := &node{view: root.view, path: ".git/info/exclude"}
	var attrOut fuse.AttrOut
	if errno := exclude.Getattr(ctx, nil, &attrOut); errno != 0 {
		t.Fatalf("Getattr exclude: got errno %v, want 0", errno)
	}

	if err := root.view.layers[1].root.Close(); err != nil {
		t.Fatal(err)
	}

	if errno := exclude.Getattr(ctx, nil, &attrOut); errno != 0 {
		t.Fatalf("Getattr exclude after close: got errno %v, want 0", errno)
	}

	if err := root.view.RefreshPaths(); err == nil {
		t.Fatal("refreshPaths with closed layer: expected error, got nil")
	}
}

func TestProjectFileCreatedWhereOverlayHasSamePath(t *testing.T) {
	root, _, shared := setupTestRefreshNode(t, true)
	scratch.Write(t, filepath.Join(shared, "conflict.txt"), "overlay")
	if err := root.view.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out fuse.EntryOut
	_, _, _, errno := root.Create(ctx, "conflict.txt", syscall.O_RDWR, 0644, &out)
	if errno == 0 {
		t.Fatalf("Create conflicting file: got errno 0, want error")
	}
}

func TestOverlayDeletedPathsAreRefreshed(t *testing.T) {
	root, _, shared := setupTestRefreshNode(t, false)
	scratch.Write(t, filepath.Join(shared, "temp.txt"), "temporary")
	if err := root.view.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	if _, err := root.view.resolve("temp.txt"); err != nil {
		t.Fatalf("resolve temp.txt: %v", err)
	}
	if err := root.view.layers[1].root.Remove("temp.txt"); err != nil {
		t.Fatal(err)
	}
	if err := root.view.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	if _, err := root.view.resolve("temp.txt"); err == nil {
		t.Fatalf("resolve temp.txt after delete: expected error")
	}
}

func TestExcludeCleanSkipsWalkWhenAlreadyClean(t *testing.T) {
	root, _, _ := setupTestRefreshNode(t, true)
	if err := root.view.refreshExclude(); err != nil {
		t.Fatal(err)
	}
	if err := root.view.layers[1].root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.view.refreshExclude(); err != nil {
		t.Fatalf("refreshExclude when already clean should return nil, got %v", err)
	}
}

func TestChangedDetectsOverlayInProject(t *testing.T) {
	root, _, shared := setupTestRefreshNode(t, false)
	scratch.Write(t, filepath.Join(shared, "overlay.txt"), "from overlay")
	if err := root.view.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out fuse.EntryOut
	_, _, _, errno := root.Create(ctx, "overlay.txt", syscall.O_RDWR, 0644, &out)
	if errno == 0 {
		t.Fatalf("Create overlay.txt when it exists in overlay: got errno 0, want error")
	}
}
