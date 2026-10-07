package gitexclude

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMain(m *testing.M) {
	scratch.Main(m)
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
}

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
			got, err := Pattern(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Pattern(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("expected error %q to contain %q", err.Error(), tt.errSubstr)
				}
			} else if got != tt.want {
				t.Errorf("Pattern(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestOpenExcludeNoGit(t *testing.T) {
	tmpDir := t.TempDir()
	ge, err := Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("Open error = %v", err)
	}
	if ge != nil {
		t.Errorf("expected nil File for non-git directory, got %v", ge)
	}
}

func TestExcludeNativeHandleFailures(t *testing.T) {
	var absent *File
	if clone, err := absent.Clone(); err != nil || clone != nil {
		t.Fatalf("non-Git clone: %v %v", clone, err)
	}
	project := t.TempDir()
	initGitRepo(t, project)
	g, err := Open(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.root.Close(); err != nil {
		t.Fatal(err)
	}
	if clone, err := g.Clone(); err == nil {
		clone.Close()
		t.Fatal("closed exclude handle cloned")
	}
	if err := g.Update([]string{"/ignored"}); err == nil {
		t.Fatal("closed exclude handle updated")
	}
}

func TestOpenExcludeErrors(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	t.Run("non_git_directory", func(t *testing.T) {
		fakeGitDir := filepath.Join(tmpDir, "fake")
		scratch.Mkdir(t, filepath.Join(fakeGitDir, ".git"))
		_, err := Open(ctx, fakeGitDir)
		if err == nil || !strings.Contains(err.Error(), "locate Git exclude file") {
			t.Errorf("expected locate Git exclude file error, got %v", err)
		}
	})

	t.Run("file_as_directory", func(t *testing.T) {
		fileAsDir := filepath.Join(tmpDir, "file.txt")
		scratch.Write(t, fileAsDir, "test")
		badPath := filepath.Join(fileAsDir, "sub")
		_, err := Open(ctx, badPath)
		if err == nil {
			t.Errorf("expected error from Lstat on invalid path, got nil")
		}
	})
}

func TestGitExcludeLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	initGitRepo(t, tmpDir)

	ctx := context.Background()
	ge, err := Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open error = %v", err)
	}
	if ge == nil {
		t.Fatal("expected non-nil File for git repository")
	}

	excludePath := filepath.Join(tmpDir, ".git", "info", "exclude")

	// Update with initial patterns
	patterns := []string{"/overlay.txt", "/custom-dir"}
	if err := ge.Update(patterns); err != nil {
		t.Fatalf("Update error = %v", err)
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
	if err := ge.Update(patterns); err != nil {
		t.Fatalf("idempotent Update error = %v", err)
	}

	// Update with new patterns replaces block
	newPatterns := []string{"/different.txt"}
	if err := ge.Update(newPatterns); err != nil {
		t.Fatalf("Update with new patterns error = %v", err)
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
	err = ge.Update([]string{"/fail.txt"})
	if err == nil || !strings.Contains(err.Error(), "managed Git exclude block changed") {
		t.Errorf("expected error refusing to overwrite tampered exclude file, got %v", err)
	}

	// Close should handle nil cleanly
	var nilGe *File
	if err := nilGe.Close(); err != nil {
		t.Errorf("nilGe.Close() error = %v", err)
	}

	// Update with nil twice (second is no-op returning nil)
	ge2, err := Open(ctx, tmpDir)
	if err == nil && ge2 != nil {
		if err := ge2.Update(nil); err != nil {
			t.Errorf("first Update(nil) failed: %v", err)
		}
		if err := ge2.Update(nil); err != nil {
			t.Errorf("Update(nil) idempotent failed: %v", err)
		}
		if err := ge2.Close(); err != nil {
			t.Errorf("ge2.Close() failed: %v", err)
		}
	}
}

func TestRestoreRemovesRecordedBlock(t *testing.T) {
	tmpDir := t.TempDir()
	scratch.GitRepo(t, tmpDir)
	excludePath := filepath.Join(tmpDir, ".git", "info", "exclude")
	original := scratch.Read(t, excludePath)

	f, err := Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Capture the block through OnUpdate
	var recordedBlock []byte
	f.OnUpdate = func(block []byte) error {
		recordedBlock = append([]byte(nil), block...)
		return nil
	}

	if err := f.Update([]string{"/generated.md"}); err != nil {
		t.Fatal(err)
	}

	if len(recordedBlock) == 0 {
		t.Fatal("OnUpdate was not called")
	}

	// Restore should remove the recorded block
	if err := Restore(f.Path(), recordedBlock); err != nil {
		t.Fatal(err)
	}

	restored := scratch.Read(t, excludePath)
	if !bytes.Equal(original, restored) {
		t.Errorf("restored file differs from original:\noriginal: %q\nrestored: %q", original, restored)
	}

	// Test error case: edit the block on disk before restore
	f2, err := Open(context.Background(), tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()

	f2.OnUpdate = func(block []byte) error {
		recordedBlock = append([]byte(nil), block...)
		return nil
	}

	if err := f2.Update([]string{"/another.md"}); err != nil {
		t.Fatal(err)
	}

	// Edit the block on disk
	content := scratch.Read(t, excludePath)
	edited := strings.Replace(string(content), "/another.md", "/edited.md", 1)
	scratch.Write(t, excludePath, edited)

	// Restore with the original block should error
	err = Restore(f2.Path(), recordedBlock)
	if err == nil {
		t.Fatal("Restore should error when block was changed on disk")
	}
	if !strings.Contains(err.Error(), "managed Git exclude block changed") {
		t.Errorf("expected block changed error, got: %v", err)
	}
}

// TestGitExcludeUpdateWriteError tests that an exclude file update fails when the Git info directory is read-only,
// and succeeds once the directory is writable again.
func TestGitExcludeUpdateWriteError(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()

	initGitRepo(t, tmpDir)

	// Open exclude to initialize it
	ge, err := Open(ctx, tmpDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := ge.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})

	infoDir := filepath.Join(tmpDir, ".git", "info")
	excludePath := filepath.Join(infoDir, "exclude")

	// Remove the exclude file and make the info directory read-only
	// so that the next update cannot create/write the exclude file
	if err := os.Remove(excludePath); err != nil {
		t.Fatalf("remove exclude file: %v", err)
	}

	if err := os.Chmod(infoDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(infoDir, 0755); err != nil {
			t.Errorf("restore info dir permissions: %v", err)
		}
	})

	// Try to update - should fail because info dir is read-only
	err = ge.Update([]string{"/pattern.txt"})
	if err == nil {
		t.Errorf("Update with read-only info dir should fail")
	}

	// Restore permissions and try again - should succeed
	if err := os.Chmod(infoDir, 0755); err != nil {
		t.Fatal(err)
	}

	err = ge.Update([]string{"/pattern.txt"})
	if err != nil {
		t.Fatalf("Update after restoring permissions failed: %v", err)
	}
}
