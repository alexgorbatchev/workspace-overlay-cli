// Package holder finds and stops the processes that keep a mount busy.
package holder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/subprocess"
)

// pollInterval is how often Stop checks whether the processes have exited.
const pollInterval = 50 * time.Millisecond

// Process is one process that uses a mount.
type Process struct {
	PID int
	// Command is the process's command line.
	Command string
	// Dir is the process's working directory, or "" when it cannot be read.
	Dir string
}

// Find lists the processes that use the file system mounted at target: those
// with a working directory, root, executable, open file or mapping on it. It
// lists none when nothing is mounted at target, so a path on an ordinary disk
// never names every process that uses that disk. The search runs in the fuser
// command, so a mount that this process serves is never touched from here.
func Find(ctx context.Context, target string) ([]Process, error) {
	cmd := exec.CommandContext(ctx, "fuser", "--ismountpoint", "--mount", target)
	var diagnostics strings.Builder
	cmd.Stderr = &diagnostics
	data, err := subprocess.Output(ctx, cmd)
	if err != nil {
		// fuser exits 1 when it finds no process, and also when it fails.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(bytes.TrimSpace(data)) != 0 {
			return nil, fmt.Errorf("list processes using %s: %w: %s", target, err, strings.TrimSpace(diagnostics.String()))
		}
	}
	var found []Process
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("list processes using %s: unexpected fuser output %q", target, field)
		}
		if pid == os.Getpid() {
			continue
		}
		if p, ok := describe(pid); ok {
			found = append(found, p)
		}
	}
	return found, nil
}

// describe reads what /proc says about pid. It reports false for a process
// that has exited.
func describe(pid int) (Process, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	line, err := os.ReadFile(dir + "/cmdline")
	if err != nil {
		return Process{}, false
	}
	command := strings.TrimSpace(strings.ReplaceAll(string(line), "\x00", " "))
	if command == "" {
		// Kernel threads and zombies have no command line.
		name, err := os.ReadFile(dir + "/comm")
		if err != nil {
			return Process{}, false
		}
		command = strings.TrimSpace(string(name))
	}
	// Reading the link yields the path only; it does not visit the directory.
	cwd, err := os.Readlink(dir + "/cwd")
	if err != nil {
		cwd = ""
	}
	return Process{PID: pid, Command: command, Dir: cwd}, true
}

// Stop asks the processes to terminate, and kills those still running after
// grace. It returns once all of them have exited, or with an error naming the
// ones that have not after another grace period.
func Stop(ctx context.Context, processes []Process, grace time.Duration) error {
	var result error
	var running []*os.Process
	for _, p := range processes {
		// On Linux the handle refers to this very process, so a later signal
		// cannot reach an unrelated process that was given the same PID.
		handle, err := os.FindProcess(p.PID)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("stop process %d: %w", p.PID, err))
			continue
		}
		defer func() { _ = handle.Release() }() // nothing to report: the handle is only being dropped
		if err := handle.Signal(syscall.SIGTERM); err != nil {
			if !errors.Is(err, os.ErrProcessDone) {
				result = errors.Join(result, fmt.Errorf("stop process %d: %w", p.PID, err))
			}
			continue
		}
		running = append(running, handle)
	}
	running = await(ctx, running, grace)
	for _, handle := range running {
		if err := handle.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			result = errors.Join(result, fmt.Errorf("kill process %d: %w", handle.Pid, err))
		}
	}
	for _, handle := range await(ctx, running, grace) {
		result = errors.Join(result, fmt.Errorf("process %d is still running", handle.Pid))
	}
	return result
}

// await waits up to limit for the processes to exit and returns those that
// are still running.
func await(ctx context.Context, running []*os.Process, limit time.Duration) []*os.Process {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		var alive []*os.Process
		for _, handle := range running {
			if !exited(handle) {
				alive = append(alive, handle)
			}
		}
		running = alive
		if len(running) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return running
		case <-deadline.C:
			return running
		case <-tick.C:
		}
	}
}

// exited reports whether the process has ended. A process that ended but has
// not been collected by its parent still accepts signals; it holds nothing
// open, so it counts as ended.
func exited(handle *os.Process) bool {
	if err := handle.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(handle.Pid) + "/stat")
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	// The state follows the command name, which is the only field that can
	// contain a parenthesis.
	_, state, found := strings.Cut(string(stat[bytes.LastIndexByte(stat, ')')+1:]), " ")
	return found && strings.HasPrefix(state, "Z")
}
