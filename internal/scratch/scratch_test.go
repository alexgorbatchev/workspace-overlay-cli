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
	if returned := Write(t, name, content); returned != name {
		t.Errorf("Write returned %q, want %q", returned, name)
	}
	if got := string(Read(t, name)); got != content {
		t.Errorf("Read returned %q, want %q", got, content)
	}
}

func TestRead(t *testing.T) {
	name := filepath.Join(t.TempDir(), "file.txt")
	content := "test content"
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if got := string(Read(t, name)); got != content {
		t.Errorf("Read returned %q, want %q", got, content)
	}
}

func TestMkdir(t *testing.T) {
	name := filepath.Join(t.TempDir(), "a", "b", "c")
	if returned := Mkdir(t, name); returned != name {
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
	output, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-list failed: %v", err)
	}
	if count := strings.TrimSpace(string(output)); count != "1" {
		t.Errorf("expected 1 commit, got: %s", count)
	}
}

func TestMainSetsEnvironment(t *testing.T) {
	tmpdir := os.Getenv("TMPDIR")
	if tmpdir == "" {
		t.Fatal("TMPDIR not set")
	}
	if !strings.HasSuffix(tmpdir, "/.tmp") {
		t.Errorf("TMPDIR should end with /.tmp, got: %s", tmpdir)
	}
	xdgStateHome := os.Getenv("XDG_STATE_HOME")
	if xdgStateHome == "" {
		t.Fatal("XDG_STATE_HOME not set")
	}
	if !strings.HasSuffix(xdgStateHome, "/.tmp/state") {
		t.Errorf("XDG_STATE_HOME should end with /.tmp/state, got: %s", xdgStateHome)
	}
}
