package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/holder"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

// served is one overlay mount served by this test process.
type served struct {
	plan   mountPlan
	stop   context.CancelFunc
	result chan error
}

// serveMount mounts a view and serves it until the test stops it.
func serveMount(t *testing.T, confirm Confirm) served {
	t.Helper()
	view, target, _, _ := sessionView(t)
	if err := view.RefreshPaths(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := served{plan: mountPlan{target: target, view: view, confirm: confirm}, stop: cancel, result: make(chan error, 1)}
	go func() { s.result <- serve(ctx, s.plan) }()
	waitMount(t, target, overlayfs.FilesystemType)
	t.Cleanup(func() {
		cancel()
		// A test that left the mount busy has ended the process that used it.
		if err := unmountOverlay(context.Background(), target, nil); err != nil {
			t.Errorf("unmount %s after the test: %v", target, err)
		}
	})
	return s
}

// finish returns what serving the mount ended with.
func (s served) finish(t *testing.T) error {
	t.Helper()
	select {
	case err := <-s.result:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the mount is still being served")
		return nil
	}
}

// occupant is a process whose working directory is inside a mount.
type occupant struct {
	pid    int
	exited chan struct{}
}

// occupy starts a process in dir. The process ends with the test.
func occupy(t *testing.T, dir string) occupant {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	o := occupant{pid: cmd.Process.Pid, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait() // the test ends the process, so how it ended says nothing
		close(o.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // it may be gone already
		<-o.exited
	})
	return o
}

func (o occupant) running() bool {
	select {
	case <-o.exited:
		return false
	default:
		return true
	}
}

// wantBusy checks that err names the occupant as the reason target could not
// be unmounted.
func wantBusy(t *testing.T, err error, target string, o occupant) {
	t.Helper()
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("unmount = %v, want a BusyError", err)
	}
	want := holder.Process{PID: o.pid, Command: "sleep 60", Dir: target}
	if busy.Target != target || len(busy.Holders) != 1 || busy.Holders[0] != want {
		t.Fatalf("BusyError = %+v, want target %s held by %+v", busy, target, want)
	}
	line := fmt.Sprintf("%s is in use by:\n  pid %d: sleep 60 (in %s)", pathname.Display(target), o.pid, pathname.Display(target))
	if !strings.Contains(err.Error(), line) {
		t.Fatalf("message %q does not name the process as %q", err, line)
	}
}

// Stopping a mount that a process still uses cannot unmount it. Unless the
// user agrees to stop that process, the failure names it and leaves it alone.
func TestStoppedMountNamesProcessesThatUseIt(t *testing.T) {
	tests := []struct {
		name    string
		confirm Confirm
	}{
		{"nobody to ask", nil},
		{"declined", func(string, []holder.Process) bool { return false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mount := serveMount(t, tt.confirm)
			o := occupy(t, mount.plan.target)

			mount.stop()
			err := mount.finish(t)

			wantBusy(t, err, mount.plan.target, o)
			if !o.running() {
				t.Fatal("the process was stopped without consent")
			}
			// The cleanup can unmount only once the process is gone.
			if err := holder.Stop(context.Background(), []holder.Process{{PID: o.pid}}, time.Second); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStoppedMountStopsProcessesOnceConfirmed(t *testing.T) {
	var asked []holder.Process
	var askedTarget string
	mount := serveMount(t, func(target string, holders []holder.Process) bool {
		askedTarget, asked = target, holders
		return true
	})
	o := occupy(t, mount.plan.target)

	mount.stop()

	if err := mount.finish(t); err != nil {
		t.Fatalf("stopping the mount = %v", err)
	}
	if askedTarget != mount.plan.target || len(asked) != 1 || asked[0].PID != o.pid {
		t.Fatalf("asked about %s held by %+v, want %s held by pid %d", askedTarget, asked, mount.plan.target, o.pid)
	}
	if o.running() {
		t.Fatal("the process that used the mount is still running")
	}
	waitMount(t, mount.plan.target, "")
}

// The unmount command meets the same busy mount from another process.
func TestUnmountOverlayHandlesBusyMount(t *testing.T) {
	mount := serveMount(t, nil)
	target := mount.plan.target
	o := occupy(t, target)

	err := unmountOverlay(context.Background(), target, nil)

	wantBusy(t, err, target, o)
	if !strings.Contains(err.Error(), "unmount "+target) {
		t.Fatalf("message %q lost the failed unmount", err)
	}
	waitMount(t, target, overlayfs.FilesystemType)

	agree := func(string, []holder.Process) bool { return true }
	if err := unmountOverlay(context.Background(), target, agree); err != nil {
		t.Fatalf("confirmed unmount = %v", err)
	}
	waitMount(t, target, "")
	if o.running() {
		t.Fatal("the process that used the mount is still running")
	}
	if err := mount.finish(t); err != nil {
		t.Fatalf("serving ended with %v", err)
	}
}

// Without fuser the processes cannot be listed, and the failure says so
// instead of asking about nobody.
func TestBusyMountWithoutFuserSaysWhatIsMissing(t *testing.T) {
	mount := serveMount(t, func(string, []holder.Process) bool {
		t.Error("asked to stop processes that could not be listed")
		return false
	})
	target := mount.plan.target
	o := occupy(t, target)
	tools := t.TempDir()
	for _, name := range []string{"findmnt", "fusermount3"} {
		original, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(original, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", tools)

	err := unmountOverlay(context.Background(), target, mount.plan.confirm)

	t.Setenv("PATH", path)
	want := "install fuser to see which processes use " + pathname.Display(target)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("unmount without fuser = %v, want it to say %q", err, want)
	}
	if err := holder.Stop(context.Background(), []holder.Process{{PID: o.pid}}, time.Second); err != nil {
		t.Fatal(err)
	}
}
