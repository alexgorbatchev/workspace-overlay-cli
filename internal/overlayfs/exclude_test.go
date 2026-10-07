package overlayfs

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"workspace-overlay/internal/gitexclude"
	"workspace-overlay/internal/scratch"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
}

func TestRefreshExclude(t *testing.T) {
	ctx := context.Background()

	t.Run("nil_exclude", func(t *testing.T) {
		v := &View{}
		if err := v.refreshExclude(); err != nil {
			t.Fatalf("refreshExclude on nil exclude view error = %v", err)
		}
	})

	t.Run("real_layers", func(t *testing.T) {
		tmpDir := t.TempDir()
		baseDir := filepath.Join(tmpDir, "base")
		sharedDir := filepath.Join(tmpDir, "shared")
		specificDir := filepath.Join(tmpDir, "specific")

		if err := os.MkdirAll(baseDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(sharedDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(specificDir, "nested"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(sharedDir, ".git"), 0755); err != nil {
			t.Fatal(err)
		}

		initGitRepo(t, baseDir)

		if err := os.WriteFile(filepath.Join(baseDir, "existing.txt"), []byte("base"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sharedDir, "existing.txt"), []byte("shared"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sharedDir, "shared-only.txt"), []byte("shared only"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(specificDir, "nested", "spec.txt"), []byte("spec"), 0644); err != nil {
			t.Fatal(err)
		}

		ge, err := gitexclude.Open(ctx, baseDir)
		if err != nil {
			t.Fatalf("gitexclude.Open: %v", err)
		}

		baseRoot, err := os.OpenRoot(baseDir)
		if err != nil {
			t.Fatal(err)
		}
		sharedRoot, err := os.OpenRoot(sharedDir)
		if err != nil {
			t.Fatal(err)
		}
		specificRoot, err := os.OpenRoot(specificDir)
		if err != nil {
			t.Fatal(err)
		}

		excludePath := filepath.Join(baseDir, ".git", "info", "exclude")
		// Read the original exclude file before opening gitexclude
		originalContent := scratch.Read(t, excludePath)

		v := &View{
			exclude: ge,
			layers: []layer{
				{root: baseRoot},
				{root: sharedRoot},
				{root: specificRoot},
			},
		}
		defer func() {
			if err := ge.Close(); err != nil {
				t.Errorf("ge.Close() cleanup failed: %v", err)
			}
			v.Close()
		}()

		if err := v.refreshExclude(); err != nil {
			t.Fatalf("refreshExclude: %v", err)
		}
		data, err := os.ReadFile(excludePath)
		if err != nil {
			t.Fatalf("read exclude file: %v", err)
		}
		content := string(data)

		// existing.txt should NOT be excluded because it exists in layer 0
		if strings.Contains(content, "existing.txt") {
			t.Errorf("existing.txt should not be excluded:\n%s", content)
		}
		// shared-only.txt and nested should be excluded
		if !strings.Contains(content, "/shared-only.txt") {
			t.Errorf("shared-only.txt missing from exclude:\n%s", content)
		}
		if !strings.Contains(content, "/nested") {
			t.Errorf("/nested missing from exclude:\n%s", content)
		}

		// Close ge should remove managed block
		if err := ge.Close(); err != nil {
			t.Fatalf("close ge error: %v", err)
		}
		restored := scratch.Read(t, excludePath)
		if !bytes.Equal(originalContent, restored) {
			t.Errorf("exclude file not restored to original state after Close:\noriginal: %q\nrestored: %q", originalContent, restored)
		}
		if strings.Contains(string(restored), "# begin workspace-overlay") {
			t.Errorf("managed block still present after ge.Close():\n%s", string(restored))
		}
	})
}
