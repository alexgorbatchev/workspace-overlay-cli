package session

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/holder"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

// stopGrace is how long processes that keep a mount busy get to exit after
// being asked, before they are killed.
const stopGrace = 3 * time.Second

// Confirm is asked whether the processes that keep target mounted may be
// stopped so that the unmount can go ahead.
type Confirm func(target string, holders []holder.Process) bool

// BusyError reports an unmount that failed because processes use the mount.
type BusyError struct {
	Target  string
	Holders []holder.Process
	// Err is the failure of the unmount itself.
	Err error
}

func (e *BusyError) Error() string {
	var text strings.Builder
	fmt.Fprintf(&text, "%v; %s is in use by:", e.Err, pathname.Display(e.Target))
	for _, p := range e.Holders {
		fmt.Fprintf(&text, "\n  pid %d: %s", p.PID, p.Command)
		if p.Dir != "" {
			fmt.Fprintf(&text, " (in %s)", pathname.Display(p.Dir))
		}
	}
	return text.String()
}

func (e *BusyError) Unwrap() error { return e.Err }

// release unmounts target by calling unmount. When that fails because
// processes use the mount, it names them, and when confirm agrees it stops
// them and unmounts again.
func release(ctx context.Context, target string, confirm Confirm, unmount func() error) error {
	failure := unmount()
	if failure == nil {
		return nil
	}
	found, err := holders(ctx, target)
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%w; install fuser to see which processes use %s", failure, pathname.Display(target))
	}
	if err != nil {
		return errors.Join(failure, err)
	}
	if len(found) == 0 {
		return failure
	}
	busy := &BusyError{Target: target, Holders: found, Err: failure}
	if confirm == nil || !confirm(target, found) {
		return busy
	}
	stopErr := holder.Stop(ctx, found, stopGrace)
	// Whatever could not be stopped may still have let go of the mount.
	if err := unmount(); err != nil {
		return errors.Join(err, stopErr)
	}
	return nil
}

// holders lists the processes that use the overlay mounted at target. It
// lists none unless an overlay is mounted there both before and after the
// search, so the processes it names were found on an overlay mount.
func holders(ctx context.Context, target string) ([]holder.Process, error) {
	kind, err := mountedType(ctx, target)
	if err != nil || kind != overlayfs.FilesystemType {
		return nil, err
	}
	found, err := holder.Find(ctx, target)
	if err != nil {
		return nil, err
	}
	kind, err = mountedType(ctx, target)
	if err != nil || kind != overlayfs.FilesystemType {
		return nil, err
	}
	return found, nil
}
