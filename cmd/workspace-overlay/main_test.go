package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

func TestMain(m *testing.M) {
	dir, err := filepath.Abs("../../.tmp")
	if err == nil {
		err = os.MkdirAll(dir, 0700)
	}
	if err == nil {
		err = os.Setenv("TMPDIR", dir)
	}
	// Isolate XDG_STATE_HOME to prevent tests from writing to the real home directory.
	if err == nil {
		stateDir := filepath.Join(dir, "state")
		err = os.MkdirAll(stateDir, 0700)
	}
	if err == nil {
		err = os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func captureOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()

	origStdout := os.Stdout
	origStderr := os.Stderr

	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}

	os.Stdout = wOut
	os.Stderr = wErr

	outDone := make(chan string)
	errDone := make(chan string)
	errChan := make(chan error, 2)

	go func() {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, rOut); err != nil {
			errChan <- err
		}
		outDone <- buf.String()
	}()

	go func() {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, rErr); err != nil {
			errChan <- err
		}
		errDone <- buf.String()
	}()

	fn()

	if err := wOut.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	if err := wErr.Close(); err != nil {
		t.Fatalf("close stderr pipe: %v", err)
	}

	stdoutStr := <-outDone
	stderrStr := <-errDone

	os.Stdout = origStdout
	os.Stderr = origStderr

	if err := rOut.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	if err := rErr.Close(); err != nil {
		t.Fatalf("close stderr reader: %v", err)
	}

	select {
	case err := <-errChan:
		t.Fatalf("capture output error: %v", err)
	default:
	}

	return stdoutStr, stderrStr
}

func TestRunVersion(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := run([]string{"--version"}, stdout, stderr)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if stdout.String() != version+"\n" {
		t.Errorf("got version output %q, want %q", stdout.String(), version+"\n")
	}
}

func TestRunSkill(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	err := run([]string{"skill"}, stdout, stderr)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if stdout.String() != skill {
		t.Errorf("skill command output did not match embedded skill verbatim; got length %d, want %d", len(stdout.String()), len(skill))
	}

	// Extra argument must be rejected
	stdout = new(bytes.Buffer)
	stderr = new(bytes.Buffer)
	err = run([]string{"skill", "extra"}, stdout, stderr)
	if err == nil {
		t.Errorf("expected error when extra argument passed to skill command, got nil")
	}
}

func TestEmbeddedSkillModesOffline(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, mode := range []string{"0", "1"} {
		t.Run("AGENT="+mode, func(t *testing.T) {
			t.Setenv("AGENT", mode)
			stdout := new(bytes.Buffer)
			stderr := new(bytes.Buffer)
			err := run([]string{"skill"}, stdout, stderr)
			if err != nil {
				t.Fatal(err)
			}
			if stdout.String() != skill {
				t.Fatal("offline skill output differs from embedded bytes")
			}
		})
	}
}

func TestAgentHelpPaths(t *testing.T) {
	t.Setenv("AGENT", "1")
	paths := [][]string{{"--help"}, {"overlay", "--help"}, {"overlay", "mount", "--help"}, {"overlay", "unmount", "--help"}, {"overlay", "status", "--help"}, {"fixture", "--help"}, {"fixture", "create", "--help"}, {"help", "fixture"}, {"skill", "--help"}, {"help", "overlay"}}
	for _, path := range paths {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			stdout := new(bytes.Buffer)
			stderr := new(bytes.Buffer)
			err := run(path, stdout, stderr)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(stdout.String(), "ALERT: Agents must read `AGENT=1 workspace-overlay skill` before using this tool.\n") {
				t.Fatalf("missing leading alert: %s", stdout.String())
			}
		})
	}
}

func TestRunHelpAgentAlert(t *testing.T) {
	alertLine := "ALERT: Agents must read `AGENT=1 workspace-overlay skill` before using this tool."

	agentModes := []struct {
		envVal    string
		wantAlert bool
	}{
		{"1", true},
		{"true", true},
		{"yes", true},
		{"TRUE", true},
		{"0", false},
		{"", false},
		{"false", false},
	}

	for _, tc := range agentModes {
		t.Run("AGENT="+tc.envVal, func(t *testing.T) {
			t.Setenv("AGENT", tc.envVal)
			stdout := new(bytes.Buffer)
			stderr := new(bytes.Buffer)
			if err := run([]string{"--help"}, stdout, stderr); err != nil {
				t.Fatalf("run --help: %v", err)
			}

			hasAlert := strings.Contains(stdout.String(), alertLine)
			if hasAlert != tc.wantAlert {
				t.Errorf("AGENT=%q: hasAlert=%v, wantAlert=%v\noutput:\n%s", tc.envVal, hasAlert, tc.wantAlert, stdout.String())
			}
		})
	}
}

func TestRunHelpRootWithoutArgs(t *testing.T) {
	t.Setenv("AGENT", "")
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)

	if err := run([]string{}, stdout, stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if !strings.Contains(stdout.String(), "workspace-overlay") || !strings.Contains(stdout.String(), "overlay") {
		t.Errorf("expected root help output, got: %s", stdout.String())
	}
}

func TestMainFunction(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"workspace-overlay", "--version"}
	out, _ := captureOutput(t, func() {
		main()
	})
	if out != version+"\n" {
		t.Errorf("main() output = %q, want %q", out, version+"\n")
	}
}

func TestMainErrorSubprocess(t *testing.T) {
	if os.Getenv("TEST_MAIN_ERROR") == "1" {
		os.Args = []string{"workspace-overlay", "--unknown-flag-for-main"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMainErrorSubprocess")
	cmd.Env = append(os.Environ(), "TEST_MAIN_ERROR=1")
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit code 1 from main error branch, got %v", err)
	}
}

func TestRunOverlayCLICommands(t *testing.T) {
	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "proj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(tmpDir, "workspace-overlay.toml")
	if err := os.WriteFile(configFile, []byte("version=1\n[projects.proj]\npath='proj'\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("overlay status", func(t *testing.T) {
		stdout := new(bytes.Buffer)
		stderr := new(bytes.Buffer)
		if err := run([]string{"overlay", "status", "--config", configFile, "--project", "proj", "--worktrees=false"}, stdout, stderr); err != nil {
			t.Fatalf("overlay status error = %v", err)
		}
		expected := pathname.Display(projDir) + "\tunmounted\n"
		if stdout.String() != expected {
			t.Errorf("status output = %q, want %q", stdout.String(), expected)
		}
	})

	t.Run("overlay status with worktrees", func(t *testing.T) {
		stdout := new(bytes.Buffer)
		stderr := new(bytes.Buffer)
		if err := run([]string{"overlay", "status", "--config", configFile}, stdout, stderr); err != nil {
			t.Fatalf("overlay status with worktrees error = %v", err)
		}
		if !strings.Contains(stdout.String(), "unmounted") {
			t.Errorf("status with worktrees output = %q, want unmounted", stdout.String())
		}
	})

	t.Run("overlay unmount", func(t *testing.T) {
		stdout := new(bytes.Buffer)
		stderr := new(bytes.Buffer)
		if err := run([]string{"overlay", "unmount", "--config", configFile}, stdout, stderr); err != nil {
			t.Fatalf("overlay unmount error = %v", err)
		}
	})

	t.Run("reject unknown project", func(t *testing.T) {
		stdout := new(bytes.Buffer)
		stderr := new(bytes.Buffer)
		if err := run([]string{"overlay", "mount", "--config", configFile, "--project", "missing"}, stdout, stderr); err == nil {
			t.Errorf("expected error for unknown project, got nil")
		}
	})

	t.Run("invalid flag", func(t *testing.T) {
		stdout := new(bytes.Buffer)
		stderr := new(bytes.Buffer)
		if err := run([]string{"--invalid-flag"}, stdout, stderr); err == nil {
			t.Errorf("expected error for invalid flag, got nil")
		}
	})
}

func TestSkillOutputFailure(t *testing.T) {
	// Create a writer that always fails
	failWriter := &failingWriter{}
	stderr := new(bytes.Buffer)
	err := run([]string{"skill"}, failWriter, stderr)
	if err == nil {
		t.Fatal("failing output stream accepted")
	}
}

type failingWriter struct{}

func (fw *failingWriter) Write(p []byte) (n int, err error) {
	return 0, fmt.Errorf("write failed")
}
