// Package scratch creates the temporary files and repositories tests work on.
package scratch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// statePrefix names the per-process state directories inside .tmp.
const statePrefix = "state-"

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

// Main runs a package's tests with temporary files and tool state kept inside
// the module's .tmp directory, never in the user's home or the system
// temporary directory. Tool state gets a directory of its own that is removed
// when the tests finish, so nothing a test leaves behind outlives the run.
func Main(m *testing.M) {
	cleanup, err := isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// isolate points TMPDIR and XDG_STATE_HOME into the module's .tmp directory
// and returns the cleanup to run after the tests.
func isolate() (cleanup func() error, err error) {
	moduleRoot, err := findModuleRoot()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(moduleRoot, ".tmp")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Setenv("TMPDIR", dir); err != nil {
		return nil, err
	}
	// A test that re-executes its own binary must share the state of the
	// process that started it, which also owns the directory and removes it.
	if inherited := os.Getenv("XDG_STATE_HOME"); filepath.Dir(inherited) == dir && strings.HasPrefix(filepath.Base(inherited), statePrefix) {
		return func() error { return nil }, nil
	}
	state, err := os.MkdirTemp(dir, statePrefix)
	if err != nil {
		return nil, err
	}
	if err := os.Setenv("XDG_STATE_HOME", state); err != nil {
		return nil, err
	}
	return func() error { return os.RemoveAll(state) }, nil
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
