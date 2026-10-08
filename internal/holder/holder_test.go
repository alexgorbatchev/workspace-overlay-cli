package holder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMain(m *testing.M) {
	scratch.Main(m)
}

// child is a process started by a test.
type child struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

// start runs script in a shell from dir and returns once it has printed a
// line, which tells the test that the script has finished setting up.
func start(t *testing.T, dir, script string) child {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := child{cmd: cmd, exited: make(chan struct{})}
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		t.Fatalf("script never became ready: %v", err)
	}
	go func() {
		_ = cmd.Wait() // the test ends the script, so how it ended says nothing
		close(c.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // it may be gone already
		<-c.exited
	})
	return c
}

func (c child) process() Process { return Process{PID: c.cmd.Process.Pid} }

func (c child) await(t *testing.T) {
	t.Helper()
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("process is still running")
	}
}

// fakeFuser puts a fuser on PATH that runs script instead of searching.
func fakeFuser(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fuser"), []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A directory on an ordinary disk is used by every process on that disk.
// None of them holds a mount, so none may be named.
func TestFindNamesNobodyWhereNothingIsMounted(t *testing.T) {
	dir := t.TempDir()
	start(t, dir, "echo ready; exec sleep 60")

	found, err := Find(context.Background(), dir)

	if err != nil || len(found) != 0 {
		t.Fatalf("Find on an unmounted directory = %+v, %v; want nobody", found, err)
	}
}

func TestFindDescribesReportedProcesses(t *testing.T) {
	dir := t.TempDir()
	sleeper := start(t, dir, "echo ready; exec sleep 60")
	finished := start(t, dir, "echo ready")
	finished.await(t)
	// The search reports the sleeper, a process that has exited since, and
	// its own caller, which is this test.
	fakeFuser(t, fmt.Sprintf("echo ' %d %d' $PPID", sleeper.cmd.Process.Pid, finished.cmd.Process.Pid))

	found, err := Find(context.Background(), dir)

	want := []Process{{PID: sleeper.cmd.Process.Pid, Command: "sleep 60", Dir: dir}}
	if err != nil || len(found) != 1 || found[0] != want[0] {
		t.Fatalf("Find = %+v, %v; want %+v", found, err, want)
	}
}

func TestFindReportsFailedSearch(t *testing.T) {
	tests := []struct {
		name, script, want string
	}{
		{"search failed", "echo broken >&2; exit 2", "broken"},
		{"failed with output", "echo 12; exit 1", "exit status 1"},
		{"unexpected output", "echo twelve", `unexpected fuser output "twelve"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeFuser(t, tt.script)
			found, err := Find(context.Background(), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) || found != nil {
				t.Fatalf("Find = %+v, %v; want an error containing %q", found, err, tt.want)
			}
		})
	}
}

func TestFindWithoutFuserSaysItIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Find(context.Background(), t.TempDir()); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("Find without fuser = %v, want exec.ErrNotFound", err)
	}
}

func TestStopEndsProcesses(t *testing.T) {
	tests := []struct {
		name, script string
	}{
		{"exits when asked", "echo ready; exec sleep 60"},
		{"ignores the request", `trap "" TERM; echo ready; while :; do sleep 0.05; done`},
		{"already exited", "echo ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := start(t, t.TempDir(), tt.script)
			if err := Stop(context.Background(), []Process{c.process()}, 300*time.Millisecond); err != nil {
				t.Fatalf("Stop = %v", err)
			}
			c.await(t)
		})
	}
}

// Stopping must not wait for a parent to collect a process that has ended.
func TestStopDoesNotWaitForUncollectedProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() }) // collects the process the test left uncollected

	began := time.Now()
	err := Stop(context.Background(), []Process{{PID: cmd.Process.Pid}}, 30*time.Second)

	if err != nil || time.Since(began) > 10*time.Second {
		t.Fatalf("Stop = %v after %v; want a prompt success", err, time.Since(began))
	}
}
