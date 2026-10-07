package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := filepath.Abs("../../.tmp")
	if err == nil {
		err = os.MkdirAll(dir, 0700)
	}
	if err == nil {
		err = os.Setenv("TMPDIR", dir)
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

	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rOut)
		outDone <- buf.String()
	}()

	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, rErr)
		errDone <- buf.String()
	}()

	fn()

	_ = wOut.Close()
	_ = wErr.Close()

	stdoutStr := <-outDone
	stderrStr := <-errDone

	os.Stdout = origStdout
	os.Stderr = origStderr

	_ = rOut.Close()
	_ = rErr.Close()

	return stdoutStr, stderrStr
}

func TestProjectPath(t *testing.T) {
	tests := []struct {
		name      string
		root      string
		project   string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "valid relative",
			root:    "..",
			project: "alpha",
			wantErr: false,
		},
		{
			name:    "valid current dir",
			root:    ".",
			project: "my-app",
			wantErr: false,
		},
		{
			name:      "empty project",
			root:      ".",
			project:   "",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
		{
			name:      "dot project",
			root:      ".",
			project:   ".",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
		{
			name:      "dot dot project",
			root:      ".",
			project:   "..",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
		{
			name:      "slash in project",
			root:      ".",
			project:   "nested/dir",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
		{
			name:      "backslash in project",
			root:      ".",
			project:   "nested\\dir",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := projectPath(tt.root, tt.project)
			if (err != nil) != tt.wantErr {
				t.Fatalf("projectPath(%q, %q) error = %v, wantErr %v", tt.root, tt.project, err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.errSubstr)
				}
			} else {
				if !filepath.IsAbs(got) {
					t.Errorf("expected absolute path, got %q", got)
				}
				expected := filepath.Clean(filepath.Join(tt.root, tt.project))
				expectedAbs, _ := filepath.Abs(expected)
				if got != expectedAbs {
					t.Errorf("got %q, want %q", got, expectedAbs)
				}
			}
		})
	}
}

func TestDisplayPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("get home dir: %v", err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "exact home dir",
			path: home,
			want: "~",
		},
		{
			name: "path inside home",
			path: filepath.Join(home, "projects", "repo"),
			want: "~" + string(filepath.Separator) + filepath.Join("projects", "repo"),
		},
		{
			name: "path outside home",
			path: "/var/log/messages",
			want: "/var/log/messages",
		},
		{
			name: "parent of home",
			path: filepath.Dir(home),
			want: filepath.Dir(home),
		},
		{
			name: "sibling of home",
			path: filepath.Join(filepath.Dir(home), "sibling_dir"),
			want: filepath.Join(filepath.Dir(home), "sibling_dir"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := displayPath(tt.path)
			if got != tt.want {
				t.Errorf("displayPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}

	t.Run("no home dir", func(t *testing.T) {
		t.Setenv("HOME", "")
		got := displayPath("/some/path")
		if got != "/some/path" {
			t.Errorf("displayPath with no HOME = %q, want /some/path", got)
		}
	})
}

func TestRunVersion(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"workspace-overlay", "--version"}
	out, _ := captureOutput(t, func() {
		if err := run(); err != nil {
			t.Fatalf("run() error = %v", err)
		}
	})

	if out != "0.1.0\n" {
		t.Errorf("got version output %q, want stable version 0.1.0 followed by a newline", out)
	}
}

func TestRunSkill(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	os.Args = []string{"workspace-overlay", "skill"}
	out, _ := captureOutput(t, func() {
		if err := run(); err != nil {
			t.Fatalf("run() error = %v", err)
		}
	})

	if out != skill {
		t.Errorf("skill command output did not match embedded skill verbatim; got length %d, want %d", len(out), len(skill))
	}

	// Extra argument must be rejected
	os.Args = []string{"workspace-overlay", "skill", "extra"}
	err := run()
	if err == nil {
		t.Errorf("expected error when extra argument passed to skill command, got nil")
	}
}

func TestEmbeddedSkillModesOffline(t *testing.T) {
	t.Chdir(t.TempDir())
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	for _, mode := range []string{"0", "1"} {
		t.Run("AGENT="+mode, func(t *testing.T) {
			t.Setenv("AGENT", mode)
			os.Args = []string{"workspace-overlay", "skill"}
			out, _ := captureOutput(t, func() {
				if err := run(); err != nil {
					t.Fatal(err)
				}
			})
			if out != skill {
				t.Fatal("offline skill output differs from embedded bytes")
			}
		})
	}
}

func TestAgentHelpPaths(t *testing.T) {
	t.Setenv("AGENT", "1")
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	paths := [][]string{{"--help"}, {"overlay", "--help"}, {"overlay", "mount", "--help"}, {"overlay", "unmount", "--help"}, {"overlay", "status", "--help"}, {"skill", "--help"}, {"help", "overlay"}}
	for _, path := range paths {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			os.Args = append([]string{"workspace-overlay"}, path...)
			out, _ := captureOutput(t, func() {
				if err := run(); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.HasPrefix(out, "ALERT: Agents must read `AGENT=1 workspace-overlay skill` before using this tool.\n") {
				t.Fatalf("missing leading alert: %s", out)
			}
		})
	}
}

func TestRunHelpAgentAlert(t *testing.T) {
	origArgs := os.Args
	origAgent := os.Getenv("AGENT")
	defer func() {
		os.Args = origArgs
		if origAgent != "" {
			_ = os.Setenv("AGENT", origAgent)
		} else {
			_ = os.Unsetenv("AGENT")
		}
	}()

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
			if tc.envVal != "" {
				_ = os.Setenv("AGENT", tc.envVal)
			} else {
				_ = os.Unsetenv("AGENT")
			}
			os.Args = []string{"workspace-overlay", "--help"}

			out, _ := captureOutput(t, func() {
				_ = run()
			})

			hasAlert := strings.Contains(out, alertLine)
			if hasAlert != tc.wantAlert {
				t.Errorf("AGENT=%q: hasAlert=%v, wantAlert=%v\noutput:\n%s", tc.envVal, hasAlert, tc.wantAlert, out)
			}
		})
	}
}

func TestRunHelpRootWithoutArgs(t *testing.T) {
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	_ = os.Unsetenv("AGENT")
	os.Args = []string{"workspace-overlay"}

	out, _ := captureOutput(t, func() {
		if err := run(); err != nil {
			t.Fatalf("run() error = %v", err)
		}
	})

	if !strings.Contains(out, "workspace-overlay") || !strings.Contains(out, "overlay") {
		t.Errorf("expected root help output, got: %s", out)
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
	origArgs := os.Args
	defer func() { os.Args = origArgs }()

	tmpDir := t.TempDir()
	projDir := filepath.Join(tmpDir, "proj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(tmpDir, "workspace-overlay.toml")
	if err := os.WriteFile(config, []byte("version=1\n[projects.proj]\npath='proj'\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. overlay status
	os.Args = []string{"workspace-overlay", "overlay", "status", "--config", config, "--project", "proj", "--worktrees=false"}
	out, _ := captureOutput(t, func() {
		if err := run(); err != nil {
			t.Fatalf("overlay status error = %v", err)
		}
	})
	if strings.TrimSpace(out) != "unmounted" {
		t.Errorf("status output = %q, want unmounted", strings.TrimSpace(out))
	}

	// 2. overlay status with --worktrees
	os.Args = []string{"workspace-overlay", "overlay", "status", "--config", config}
	out, _ = captureOutput(t, func() {
		if err := run(); err != nil {
			t.Fatalf("overlay status with worktrees error = %v", err)
		}
	})
	if !strings.Contains(out, "unmounted") {
		t.Errorf("status with worktrees output = %q, want unmounted", out)
	}

	// 3. overlay unmount
	os.Args = []string{"workspace-overlay", "overlay", "unmount", "--config", config}
	if err := run(); err != nil {
		t.Fatalf("overlay unmount error = %v", err)
	}

	// 4. Configured selection rejects unknown projects before mounting.
	os.Args = []string{"workspace-overlay", "overlay", "mount", "--config", config, "--project", "missing"}
	if err := run(); err == nil {
		t.Errorf("expected error for unknown project, got nil")
	}

	// 5. invalid flag
	os.Args = []string{"workspace-overlay", "--invalid-flag"}
	if err := run(); err == nil {
		t.Errorf("expected error for invalid flag, got nil")
	}
}

func TestLostWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := configPath(""); err == nil {
		t.Fatal("discovery accepted removed working directory")
	}
	if _, err := loadConfig(""); err == nil {
		t.Fatal("load accepted removed working directory")
	}
	if _, err := projectPath(".", "project"); err == nil {
		t.Fatal("relative project accepted removed working directory")
	}
	if _, err := mountedType(context.Background(), "."); err == nil {
		t.Fatal("mount status accepted removed working directory")
	}
	if err := unmountOverlay(context.Background(), "."); err == nil {
		t.Fatal("unmount accepted removed working directory")
	}
}

func TestSkillOutputFailure(t *testing.T) {
	args, stdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args = args; os.Stdout = stdout })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	defer closeFile(r)
	os.Stdout = w
	os.Args = []string{"workspace-overlay", "skill"}
	if err := run(); err == nil {
		t.Fatal("closed output stream accepted")
	}
}
