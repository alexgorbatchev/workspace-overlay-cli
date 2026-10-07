package scratch

import (
	"fmt"
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
	state := os.Getenv("XDG_STATE_HOME")
	if filepath.Dir(state) != tmpdir || !strings.HasPrefix(filepath.Base(state), "state-") {
		t.Fatalf("XDG_STATE_HOME = %q, want a state-* directory inside %s", state, tmpdir)
	}
	if info, err := os.Stat(state); err != nil || !info.IsDir() {
		t.Fatalf("state directory: %v", err)
	}
}

const reportStateHome = "SCRATCH_REPORT_STATE_HOME"

// TestStateHomeHelper runs in a child process and reports the state directory
// Main gave it, leaving a file behind the way an unfinished registry would.
func TestStateHomeHelper(t *testing.T) {
	if os.Getenv(reportStateHome) == "" {
		return
	}
	state := os.Getenv("XDG_STATE_HOME")
	Write(t, filepath.Join(state, "leftover"), "state")
	fmt.Printf("state-home=%s\n", state)
}

func childStateHome(t *testing.T, env []string) string {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestStateHomeHelper$")
	child.Env = append(env, reportStateHome+"=1")
	out, err := child.Output()
	if err != nil {
		t.Fatalf("child test process: %v: %s", err, out)
	}
	for line := range strings.Lines(string(out)) {
		if state, ok := strings.CutPrefix(strings.TrimSpace(line), "state-home="); ok {
			return state
		}
	}
	t.Fatalf("child reported no state directory: %s", out)
	return ""
}

func TestStateDirectoryIsRemovedWhenTestsFinish(t *testing.T) {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "XDG_STATE_HOME=") {
			env = append(env, entry)
		}
	}
	state := childStateHome(t, env)
	if state == os.Getenv("XDG_STATE_HOME") {
		t.Fatalf("an unrelated test process shared this one's state directory %s", state)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("state directory %s after its tests finished: %v, want it removed", state, err)
	}
}

// A test that re-executes its own binary, like an owner process that is later
// killed, must see the state its parent sees and must not delete it.
func TestChildProcessSharesStateDirectory(t *testing.T) {
	own := os.Getenv("XDG_STATE_HOME")
	if state := childStateHome(t, os.Environ()); state != own {
		t.Fatalf("child state directory = %s, want the parent's %s", state, own)
	}
	if got := string(Read(t, filepath.Join(own, "leftover"))); got != "state" {
		t.Fatalf("state written by the child = %q, want it kept for the parent", got)
	}
}
