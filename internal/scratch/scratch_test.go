package scratch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	Main(m)
}

func TestWrite(t *testing.T) {
	name := filepath.Join(t.TempDir(), "dir", "file.txt")
	content := "test content"
	returned := Write(t, name, content)

	if returned != name {
		t.Errorf("Write returned %q, want %q", returned, name)
	}

	data := Read(t, name)
	if string(data) != content {
		t.Errorf("Read returned %q, want %q", string(data), content)
	}
}

func TestRead(t *testing.T) {
	name := filepath.Join(t.TempDir(), "file.txt")
	content := "test content"
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	data := Read(t, name)
	if string(data) != content {
		t.Errorf("Read returned %q, want %q", string(data), content)
	}
}

func TestMkdir(t *testing.T) {
	base := t.TempDir()
	name := filepath.Join(base, "a", "b", "c")
	returned := Mkdir(t, name)

	if returned != name {
		t.Errorf("Mkdir returned %q, want %q", returned, name)
	}

	info, err := os.Stat(name)
	if err != nil {
		t.Fatalf("directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("created path is not a directory")
	}
}

func TestGitRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}

	GitRepo(t, dir)

	// Verify the repository has exactly one commit
	cmd := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-list failed: %v", err)
	}
	if strings.TrimSpace(string(output)) != "1" {
		t.Errorf("expected 1 commit, got: %s", strings.TrimSpace(string(output)))
	}
}

func TestMainSetsEnvironment(t *testing.T) {
	// These checks happen during TestMain, which has already run.
	// Verify the environment was set correctly.
	tmpdir := os.Getenv("TMPDIR")
	xdgStateHome := os.Getenv("XDG_STATE_HOME")

	if tmpdir == "" {
		t.Fatal("TMPDIR not set")
	}
	if !strings.HasSuffix(tmpdir, "/.tmp") {
		t.Errorf("TMPDIR should end with /.tmp, got: %s", tmpdir)
	}

	if xdgStateHome == "" {
		t.Fatal("XDG_STATE_HOME not set")
	}
	if !strings.HasSuffix(xdgStateHome, "/.tmp/state") {
		t.Errorf("XDG_STATE_HOME should end with /.tmp/state, got: %s", xdgStateHome)
	}
}
