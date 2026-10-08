package overlayfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func setupMarkedNodes(t *testing.T) (*node, string, string) {
	t.Helper()
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "alpha")
	sharedDir := filepath.Join(tmpDir, ".ai", "workspace")

	scratch.Write(t, filepath.Join(baseDir, "AGENTS.md"), "# Project Rules\n- Rule 1\n")
	scratch.Write(t, filepath.Join(sharedDir, "AGENTS.md"), "# Workspace Rules\n- Work rule\n")

	baseRoot, err := os.OpenRoot(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	sharedRoot, err := os.OpenRoot(sharedDir)
	if err != nil {
		t.Fatal(err)
	}

	v := &View{
		layers: []layer{
			{name: "alpha", root: baseRoot},
			{name: "workspace", root: sharedRoot},
		},
	}
	v.SetMarkers(defaultMarkerRules)
	t.Cleanup(func() { v.Close() })

	n := &node{view: v, path: "AGENTS.md"}
	return n, baseDir, sharedDir
}

func TestMarkedMergedFileRenderAndRead(t *testing.T) {
	n, _, _ := setupMarkedNodes(t)
	ctx := context.Background()

	parts, err := n.view.resolve(n.path)
	if err != nil {
		t.Fatal(err)
	}

	h, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
	if errno != 0 {
		t.Fatalf("openMerged errno = %v", errno)
	}
	handle := h.(*mergedFile)
	defer func() { _ = handle.Release(ctx) }()

	buf := make([]byte, 1024)
	res, errno := handle.Read(ctx, buf, 0)
	if errno != 0 {
		t.Fatalf("Read errno = %v", errno)
	}
	data, status := res.Bytes(buf)
	if !status.Ok() {
		t.Fatalf("Bytes status = %v", status)
	}
	content := string(data)

	// Verify project rules appear first
	if !strings.HasPrefix(content, "# Project Rules\n- Rule 1\n") {
		t.Errorf("content missing project prefix: %q", content)
	}

	// Verify marker contains overlay name and relative path (../.ai/workspace/AGENTS.md)
	expectedMarker := "<!-- BEGIN WORKSPACE-OVERLAY: workspace (../.ai/workspace/AGENTS.md) -->"
	if !strings.Contains(content, expectedMarker) {
		t.Errorf("content missing marker %q: %q", expectedMarker, content)
	}

	// Verify workspace rules inside marker
	if !strings.Contains(content, "# Workspace Rules\n- Work rule\n") {
		t.Errorf("content missing workspace rules: %q", content)
	}

	// Verify end marker
	expectedEnd := "<!-- END WORKSPACE-OVERLAY: workspace (../.ai/workspace/AGENTS.md) -->"
	if !strings.Contains(content, expectedEnd) {
		t.Errorf("content missing end marker %q: %q", expectedEnd, content)
	}
}

func TestMarkedMergedFileTwoWayWrite(t *testing.T) {
	t.Run("edit project section persists to base", func(t *testing.T) {
		n, baseDir, sharedDir := setupMarkedNodes(t)
		ctx := context.Background()
		parts, _ := n.view.resolve(n.path)

		h, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
		if errno != 0 {
			t.Fatal(errno)
		}
		handle := h.(*mergedFile)

		buf := make([]byte, 1024)
		res, _ := handle.Read(ctx, buf, 0)
		data, _ := res.Bytes(buf)

		// Modify only project section
		newContent := strings.Replace(string(data), "- Rule 1", "- Rule 1\n- Rule 2 (new project rule)", 1)
		_, errno = handle.Write(ctx, []byte(newContent), 0)
		if errno != 0 {
			t.Fatalf("Write failed: %v", errno)
		}
		if errno := handle.Release(ctx); errno != 0 {
			t.Fatalf("Release failed: %v", errno)
		}

		baseData := scratch.Read(t, filepath.Join(baseDir, "AGENTS.md"))
		if !strings.Contains(string(baseData), "- Rule 2 (new project rule)") {
			t.Errorf("base file not updated: %q", string(baseData))
		}
		sharedData := scratch.Read(t, filepath.Join(sharedDir, "AGENTS.md"))
		if strings.Contains(string(sharedData), "- Rule 2") {
			t.Errorf("shared file was incorrectly modified: %q", string(sharedData))
		}
	})

	t.Run("edit overlay section persists to overlay", func(t *testing.T) {
		n, baseDir, sharedDir := setupMarkedNodes(t)
		ctx := context.Background()
		parts, _ := n.view.resolve(n.path)

		h, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
		if errno != 0 {
			t.Fatal(errno)
		}
		handle := h.(*mergedFile)

		buf := make([]byte, 1024)
		res, _ := handle.Read(ctx, buf, 0)
		data, _ := res.Bytes(buf)

		// Modify only workspace section
		newContent := strings.Replace(string(data), "- Work rule", "- Work rule\n- Extra work rule", 1)
		_, errno = handle.Write(ctx, []byte(newContent), 0)
		if errno != 0 {
			t.Fatalf("Write failed: %v", errno)
		}
		if errno := handle.Release(ctx); errno != 0 {
			t.Fatalf("Release failed: %v", errno)
		}

		sharedData := scratch.Read(t, filepath.Join(sharedDir, "AGENTS.md"))
		if !strings.Contains(string(sharedData), "- Extra work rule") {
			t.Errorf("shared file not updated: %q", string(sharedData))
		}
		baseData := scratch.Read(t, filepath.Join(baseDir, "AGENTS.md"))
		if strings.Contains(string(baseData), "- Extra work rule") {
			t.Errorf("base file was incorrectly modified: %q", string(baseData))
		}
	})

	t.Run("corrupted marker rejected with EPERM", func(t *testing.T) {
		n, _, _ := setupMarkedNodes(t)
		ctx := context.Background()
		parts, _ := n.view.resolve(n.path)

		h, _, errno := n.openMerged(ctx, syscall.O_RDWR, parts)
		if errno != 0 {
			t.Fatal(errno)
		}
		handle := h.(*mergedFile)

		// Delete the END marker and truncate
		corrupted := "# Project Rules\n<!-- BEGIN WORKSPACE-OVERLAY: workspace -->\n# Workspace Rules\n"
		var in fuse.SetAttrIn
		in.Size = uint64(len(corrupted))
		in.Valid |= fuse.FATTR_SIZE
		var out fuse.AttrOut
		_ = handle.Setattr(ctx, &in, &out)
		_, errno = handle.Write(ctx, []byte(corrupted), 0)
		if errno != 0 {
			t.Fatalf("Write failed: %v", errno)
		}
		if errno := handle.Release(ctx); errno != syscall.EPERM {
			t.Fatalf("Release with corrupted markers returned %v, want EPERM", errno)
		}
	})
}

func TestMarkedMultipleOverlaysAndGitCaller(t *testing.T) {
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "alpha")
	workspaceDir := filepath.Join(tmpDir, ".ai", "workspace")
	personalDir := filepath.Join(tmpDir, ".ai", "personal")

	scratch.Write(t, filepath.Join(baseDir, "AGENTS.md"), "# Project Rules\n")
	scratch.Write(t, filepath.Join(workspaceDir, "AGENTS.md"), "# Workspace Rules\n")
	scratch.Write(t, filepath.Join(personalDir, "AGENTS.md"), "# Personal Preferences\n")

	baseRoot, _ := os.OpenRoot(baseDir)
	workspaceRoot, _ := os.OpenRoot(workspaceDir)
	personalRoot, _ := os.OpenRoot(personalDir)

	v := &View{
		layers: []layer{
			{name: "alpha", root: baseRoot},
			{name: "workspace", root: workspaceRoot},
			{name: "personal", root: personalRoot},
		},
	}
	v.SetMarkers(defaultMarkerRules)
	t.Cleanup(func() { v.Close() })

	root := &node{view: v, path: "."}
	n := &node{view: v, path: "AGENTS.md"}
	parts, _ := v.resolve(n.path)

	orig := readProcessComm
	defer func() { readProcessComm = orig }()
	readProcessComm = func(pid uint32) string {
		if pid == 77777 {
			return "git"
		}
		return "other"
	}

	gitCtx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 77777})
	nonGitCtx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 88888})

	// 1. Non-git reads all layers with markers and relative paths
	h, _, _ := n.openMerged(nonGitCtx, syscall.O_RDONLY, parts)
	handle := h.(*mergedFile)
	buf := make([]byte, 2048)
	res, _ := handle.Read(nonGitCtx, buf, 0)
	data, _ := res.Bytes(buf)
	content := string(data)
	_ = handle.Release(nonGitCtx)

	if !strings.Contains(content, "<!-- BEGIN WORKSPACE-OVERLAY: workspace (../.ai/workspace/AGENTS.md) -->") {
		t.Errorf("content missing workspace marker: %q", content)
	}
	if !strings.Contains(content, "<!-- BEGIN WORKSPACE-OVERLAY: personal (../.ai/personal/AGENTS.md) -->") {
		t.Errorf("content missing personal marker: %q", content)
	}
	if !strings.Contains(content, "# Workspace Rules") || !strings.Contains(content, "# Personal Preferences") {
		t.Errorf("content missing overlay content: %q", content)
	}

	// 2. Git caller reads only base project rules (no markers, no overlay text)
	hGit, _, _ := n.Open(gitCtx, syscall.O_RDONLY)
	defer func() { _ = hGit.(fs.FileReleaser).Release(gitCtx) }()
	resGit, _ := hGit.(fs.FileReader).Read(gitCtx, buf, 0)
	gitData, _ := resGit.Bytes(buf)
	gitContent := string(gitData)

	if gitContent != "# Project Rules\n" {
		t.Errorf("git caller read got %q, want '# Project Rules\\n'", gitContent)
	}

	// 3. Unlink removes base copy and falls back to overlays
	if errno := root.Unlink(nonGitCtx, "AGENTS.md"); errno != 0 {
		t.Fatalf("first unlink failed: %v", errno)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("base copy still exists after unlink")
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "AGENTS.md")); err != nil {
		t.Fatalf("workspace copy was removed: %v", err)
	}
}

func TestCustomMarkersConfiguration(t *testing.T) {
	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "alpha")
	sharedDir := filepath.Join(tmpDir, "shared")

	scratch.Write(t, filepath.Join(baseDir, "custom.txt"), "BASE\n")
	scratch.Write(t, filepath.Join(sharedDir, "custom.txt"), "SHARED\n")

	baseRoot, _ := os.OpenRoot(baseDir)
	sharedRoot, _ := os.OpenRoot(sharedDir)

	v := &View{
		layers: []layer{
			{name: "alpha", root: baseRoot},
			{name: "shared", root: sharedRoot},
		},
	}
	v.SetMarkers([]MarkerRule{
		{
			Glob:  "*.txt",
			Start: "=== START {name} ({path}) ===",
			End:   "=== END {name} ===",
		},
		{
			Glob:  "subdir/*.doc",
			Start: "=== START {name} ({path}) ===",
			End:   "=== END {name} ===",
		},
	})
	t.Cleanup(func() { v.Close() })

	n := &node{view: v, path: "custom.txt"}
	parts, _ := v.resolve(n.path)
	ctx := context.Background()

	h, _, _ := n.openMerged(ctx, syscall.O_RDONLY, parts)
	handle := h.(*mergedFile)
	buf := make([]byte, 1024)
	res, _ := handle.Read(ctx, buf, 0)
	data, _ := res.Bytes(buf)
	_ = handle.Release(ctx)

	content := string(data)
	if !strings.Contains(content, "=== START shared (") || !strings.Contains(content, "=== END shared ===") {
		t.Errorf("custom markers not rendered: %q", content)
	}
}

func TestParseMarkedDocumentBranches(t *testing.T) {
	rule := &MarkerRule{
		Glob:  "*.md",
		Start: "<!-- BEGIN {name} -->",
		End:   "<!-- END {name} -->",
	}
	layers := []layer{
		{name: "base"},
		{name: "overlay"},
	}

	t.Run("single part", func(t *testing.T) {
		parts := []contribution{{index: 0}}
		sections, err := parseMarkedDocument([]byte("hello"), parts, layers, rule)
		if err != nil || len(sections) != 1 || string(sections[0].data) != "hello" {
			t.Fatalf("single part failed: sections=%+v, err=%v", sections, err)
		}
	})

	t.Run("start marker missing", func(t *testing.T) {
		parts := []contribution{{index: 0}, {index: 1}}
		_, err := parseMarkedDocument([]byte("no markers"), parts, layers, rule)
		if err != syscall.EPERM {
			t.Errorf("got %v, want EPERM", err)
		}
	})

	t.Run("start marker without newline", func(t *testing.T) {
		parts := []contribution{{index: 0}, {index: 1}}
		_, err := parseMarkedDocument([]byte("<!-- BEGIN overlay -->no-newline"), parts, layers, rule)
		if err != syscall.EPERM {
			t.Errorf("got %v, want EPERM", err)
		}
	})

	t.Run("end marker missing", func(t *testing.T) {
		parts := []contribution{{index: 0}, {index: 1}}
		_, err := parseMarkedDocument([]byte("<!-- BEGIN overlay -->\nsome content"), parts, layers, rule)
		if err != syscall.EPERM {
			t.Errorf("got %v, want EPERM", err)
		}
	})

	t.Run("end marker without newline at eof", func(t *testing.T) {
		parts := []contribution{{index: 0}, {index: 1}}
		sections, err := parseMarkedDocument([]byte("<!-- BEGIN overlay -->\nsome content\n<!-- END overlay -->"), parts, layers, rule)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sections) != 2 || string(sections[1].data) != "some content\n" {
			t.Fatalf("unexpected sections: %+v", sections)
		}
	})

	t.Run("markerPrefix without name placeholder", func(t *testing.T) {
		if got := markerPrefix("FIXED", "foo"); got != "FIXED" {
			t.Errorf("got %q, want FIXED", got)
		}
	})

	t.Run("findMarkerRule matching path glob", func(t *testing.T) {
		v := &View{
			markers: []MarkerRule{
				{Glob: "nested/**/*.doc", Start: "S", End: "E"},
			},
		}
		if rule := v.findMarkerRule("nested/sub/file.doc"); rule == nil {
			t.Error("expected match on full path")
		}
		if rule := v.findMarkerRule("other/file.txt"); rule != nil {
			t.Error("expected no match on other path")
		}
	})
}


