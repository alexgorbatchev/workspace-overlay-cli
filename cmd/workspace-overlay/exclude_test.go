package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExcludePattern(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "simple path",
			input:   "foo/bar.txt",
			want:    "/foo/bar.txt",
			wantErr: false,
		},
		{
			name:    "escaped characters",
			input:   "file * [1] ? ! # hello.txt",
			want:    `/file\ \*\ \[1\]\ \?\ \!\ \#\ hello.txt`,
			wantErr: false,
		},
		{
			name:    "backslash escape",
			input:   `path\with\backslash`,
			want:    `/path\\with\\backslash`,
			wantErr: false,
		},
		{
			name:      "contains newline",
			input:     "bad\npath",
			wantErr:   true,
			errSubstr: "Git exclude cannot represent path",
		},
		{
			name:      "contains carriage return",
			input:     "bad\rpath",
			wantErr:   true,
			errSubstr: "Git exclude cannot represent path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := excludePattern(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("excludePattern(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("expected error %q to contain %q", err.Error(), tt.errSubstr)
				}
			} else if got != tt.want {
				t.Errorf("excludePattern(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
}

func TestOpenExcludeNoGit(t *testing.T) {
	tmpDir := t.TempDir()
	ge, err := openExclude(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("openExclude error = %v", err)
	}
	if ge != nil {
		t.Errorf("expected nil gitExclude for non-git directory, got %v", ge)
	}
}

func TestExcludeNativeHandleFailures(t *testing.T) {
	var absent *gitExclude
	if clone, err := absent.clone(); err != nil || clone != nil {
		t.Fatalf("non-Git clone: %v %v", clone, err)
	}
	project := t.TempDir()
	initGitRepo(t, project)
	g, err := openExclude(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.root.Close(); err != nil {
		t.Fatal(err)
	}
	if clone, err := g.clone(); err == nil {
		clone.close()
		t.Fatal("closed exclude handle cloned")
	}
	if err := g.update([]string{"/ignored"}); err == nil {
		t.Fatal("closed exclude handle updated")
	}
}

func TestOpenExcludeErrors(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	// 1. Directory with non-git .git directory: git rev-parse fails
	fakeGitDir := filepath.Join(tmpDir, "fake")
	_ = os.MkdirAll(filepath.Join(fakeGitDir, ".git"), 0755)
	_, err := openExclude(ctx, fakeGitDir)
	if err == nil || !strings.Contains(err.Error(), "locate Git exclude file") {
		t.Errorf("expected locate Git exclude file error, got %v", err)
	}

	// 2. Project path where .git stat returns error (ENOTDIR)
	fileAsDir := filepath.Join(tmpDir, "file.txt")
	_ = os.WriteFile(fileAsDir, []byte("test"), 0644)
	badPath := filepath.Join(fileAsDir, "sub")
	_, err = openExclude(ctx, badPath)
	if err == nil {
		t.Errorf("expected error from Lstat on invalid path, got nil")
	}
}

func TestGitExcludeLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	initGitRepo(t, tmpDir)

	ctx := context.Background()
	ge, err := openExclude(ctx, tmpDir)
	if err != nil {
		t.Fatalf("openExclude error = %v", err)
	}
	if ge == nil {
		t.Fatal("expected non-nil gitExclude for git repository")
	}

	excludePath := filepath.Join(tmpDir, ".git", "info", "exclude")

	// Update with initial patterns
	patterns := []string{"/overlay.txt", "/custom-dir"}
	if err := ge.update(patterns); err != nil {
		t.Fatalf("update error = %v", err)
	}

	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# begin "+ge.marker) ||
		!strings.Contains(content, "/overlay.txt") ||
		!strings.Contains(content, "/custom-dir") ||
		!strings.Contains(content, "# end "+ge.marker) {
		t.Fatalf("exclude content missing expected managed block:\n%s", content)
	}

	// Idempotent update with identical patterns
	if err := ge.update(patterns); err != nil {
		t.Fatalf("idempotent update error = %v", err)
	}

	// Update with new patterns replaces block
	newPatterns := []string{"/different.txt"}
	if err := ge.update(newPatterns); err != nil {
		t.Fatalf("update with new patterns error = %v", err)
	}
	data, err = os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file: %v", err)
	}
	content = string(data)
	if strings.Contains(content, "/overlay.txt") {
		t.Errorf("old pattern /overlay.txt still present in exclude file:\n%s", content)
	}
	if !strings.Contains(content, "/different.txt") {
		t.Errorf("new pattern /different.txt missing in exclude file:\n%s", content)
	}

	// Tamper with exclude block: duplicate block so bytes.Count != 1
	tampered := content + content
	if err := os.WriteFile(excludePath, []byte(tampered), 0644); err != nil {
		t.Fatalf("tamper exclude file: %v", err)
	}
	err = ge.update([]string{"/fail.txt"})
	if err == nil || !strings.Contains(err.Error(), "managed Git exclude block changed") {
		t.Errorf("expected error refusing to overwrite tampered exclude file, got %v", err)
	}

	// Close should handle nil cleanly
	var nilGe *gitExclude
	if err := nilGe.close(); err != nil {
		t.Errorf("nilGe.close() error = %v", err)
	}

	// Update with nil twice (second is no-op returning nil)
	ge2, err := openExclude(ctx, tmpDir)
	if err == nil && ge2 != nil {
		_ = ge2.update(nil)
		if err := ge2.update(nil); err != nil {
			t.Errorf("update(nil) idempotent failed: %v", err)
		}
		_ = ge2.close()
	}
}

func TestRefreshExclude(t *testing.T) {
	ctx := context.Background()

	// 1. refreshExclude on view with exclude == nil
	v := &view{}
	if err := v.refreshExclude(); err != nil {
		t.Fatalf("refreshExclude on nil exclude view error = %v", err)
	}

	// 2. refreshExclude with real layers
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

	ge, err := openExclude(ctx, baseDir)
	if err != nil {
		t.Fatalf("openExclude: %v", err)
	}

	baseRoot, _ := os.OpenRoot(baseDir)
	sharedRoot, _ := os.OpenRoot(sharedDir)
	specificRoot, _ := os.OpenRoot(specificDir)

	v = &view{
		exclude: ge,
		layers: []layer{
			{root: baseRoot},
			{root: sharedRoot},
			{root: specificRoot},
		},
	}
	defer func() {
		_ = ge.close()
		v.close()
	}()

	if err := v.refreshExclude(); err != nil {
		t.Fatalf("refreshExclude: %v", err)
	}

	excludePath := filepath.Join(baseDir, ".git", "info", "exclude")
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
	if err := ge.close(); err != nil {
		t.Fatalf("close ge error: %v", err)
	}
	data, err = os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude file after close: %v", err)
	}
	if strings.Contains(string(data), "# begin "+ge.marker) {
		t.Errorf("managed block still present after ge.close():\n%s", string(data))
	}
}
