// Package scratch creates the temporary files and repositories tests work on.
package scratch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Write creates name with content, making parent directories as needed.
func Write(t testing.TB, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return name
}

// Read returns the content of name.
func Read(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Mkdir creates the directory name and its parents.
func Mkdir(t testing.TB, name string) string {
	t.Helper()
	if err := os.MkdirAll(name, 0755); err != nil {
		t.Fatal(err)
	}
	return name
}

// GitRepo initializes a Git repository in dir with one commit.
func GitRepo(t testing.TB, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
	dummyFile := filepath.Join(dir, "init.txt")
	if err := os.WriteFile(dummyFile, []byte("init"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", dir, "add", "init.txt")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add in %s: %v: %s", dir, err, out)
	}
	cmd = exec.Command("git", "-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "initial commit")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit in %s: %v: %s", dir, err, out)
	}
}

// Main runs a package's tests with temporary files and tool state kept inside the module's .tmp directory, never in the user's home or the system temporary directory.
func Main(m *testing.M) {
	moduleRoot, err := findModuleRoot()
	if err == nil {
		dir := filepath.Join(moduleRoot, ".tmp")
		err = os.MkdirAll(dir, 0700)
		if err == nil {
			err = os.Setenv("TMPDIR", dir)
		}
		if err == nil {
			stateDir := filepath.Join(dir, "state")
			err = os.MkdirAll(stateDir, 0700)
			if err == nil {
				err = os.Setenv("XDG_STATE_HOME", stateDir)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}
