package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/fixture"
)

const (
	e2eConfig = "WORKSPACE_OVERLAY_E2E_CONFIG"
	e2eGrace  = 5 * time.Second
	e2eJoin   = 2 * e2eGrace
)

// A separate process can be killed if graceful FUSE shutdown does not finish.
// Never restore the reused fixture until Wait has joined that process.
type e2eOwner struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	done    chan struct{}
	result  error
	output  bytes.Buffer
	joined  bool
	checked bool
}

func startE2EOwner(t *testing.T, configFile string) *e2eOwner {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	o := &e2eOwner{cancel: cancel, done: make(chan struct{})}
	o.cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestE2EMountOwnerProcess$")
	o.cmd.Env = append(os.Environ(), e2eConfig+"="+configFile)
	o.cmd.Stdout, o.cmd.Stderr = &o.output, &o.output
	o.cmd.Cancel = func() error { return o.cmd.Process.Signal(syscall.SIGTERM) }
	o.cmd.WaitDelay = e2eGrace
	if err := o.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() {
		o.result = o.cmd.Wait()
		close(o.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-o.done:
			o.joined = true
			if !o.checked && o.result != nil && !errors.Is(o.result, context.Canceled) {
				t.Errorf("stop the mount owner: %v: %s", o.result, o.output.String())
			}
		case <-time.After(e2eJoin):
			t.Error("mount owner did not exit after TERM and KILL; preserve its fixture")
		}
	})
	return o
}

func (o *e2eOwner) finish(t *testing.T) error {
	t.Helper()
	select {
	case <-o.done:
		o.joined, o.checked = true, true
		return o.result
	case <-time.After(e2eJoin):
		t.Fatal("mount owner did not exit")
		return nil
	}
}

func TestE2EMountOwnerProcess(t *testing.T) {
	configFile := os.Getenv(e2eConfig)
	if configFile == "" {
		return
	}
	if err := run([]string{"overlay", "mount", "--config", configFile, "--replace"}, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
}

func TestE2EOwnerCleanupJoinsBeforeFixtureRemoval(t *testing.T) {
	for _, forced := range []bool{false, true} {
		name := "graceful"
		if forced {
			name = "forced"
		}
		t.Run(name, func(t *testing.T) {
			root, err := os.MkdirTemp(os.TempDir(), "e2e-owner-")
			if err != nil {
				t.Fatal(err)
			}
			var owner *e2eOwner
			workspace := filepath.Join(root, "workspace")
			configFile := filepath.Join(workspace, "workspace-overlay.toml")
			t.Cleanup(func() {
				if owner != nil {
					// Backstop the regression itself if its cleanup assertion fails.
					owner.cancel()
					select {
					case <-owner.done:
					case <-time.After(e2eJoin):
						t.Errorf("preserving %s: mount owner was not joined", root)
						return
					}
					ctx, cancel := context.WithTimeout(context.Background(), e2eGrace)
					defer cancel()
					var output bytes.Buffer
					if err := runContext(ctx, []string{"overlay", "unmount", "--config", configFile}, &output, &output); err != nil {
						t.Errorf("unmount before removing %s: %v: %s", root, err, output.String())
						return
					}
				}
				if err := os.RemoveAll(root); err != nil {
					t.Error(err)
				}
			})
			if _, err := fixture.Create(t.Context(), workspace); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(workspace, "alpha")
			t.Run("serve", func(t *testing.T) {
				owner = startE2EOwner(t, configFile)
				waitForMount(t, target, true)
				if forced {
					if err := owner.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
						t.Fatal(err)
					}
					// The outer test checks the expected forced-exit error after cleanup.
					owner.checked = true
				}
			})
			if owner == nil || !owner.joined {
				t.Fatal("fixture cleanup ran before its owner was joined")
			}
			if err := owner.cmd.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
				t.Fatalf("joined owner is still signalable: %v", err)
			}
			if forced {
				var exitErr *exec.ExitError
				if !errors.As(owner.result, &exitErr) || exitErr.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
					t.Fatalf("forced shutdown = %v, want SIGKILL", owner.result)
				}
			} else if isMountedOverlay(target) {
				t.Fatal("graceful owner cleanup left its mount active")
			}
		})
	}
}

func TestE2EOwnerReportsStartupFailure(t *testing.T) {
	owner := startE2EOwner(t, filepath.Join(t.TempDir(), "missing.toml"))
	err := owner.finish(t)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || owner.output.Len() == 0 {
		t.Fatalf("startup failure = %v, output %q", err, owner.output.String())
	}
}
