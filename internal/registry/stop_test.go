package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// liveOwner holds a project's lock with one recorded mount and never answers
// stop requests, like an owner that is busy.
func liveOwner(t *testing.T, root string) *Registry {
	t.Helper()
	r, err := Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "project")
	if err := r.Record(target, "", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(r.Forget(target), r.Close()); err != nil {
			t.Error(err)
		}
	})
	return r
}

func TestRequestStopReportsUnwritableStopFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	owner := liveOwner(t, root)
	if err := os.Mkdir(owner.StopFile(), 0700); err != nil {
		t.Fatal(err)
	}

	if released, err := RequestStop(context.Background(), root, "project"); err == nil {
		t.Fatalf("stop request that could not be written was accepted: released=%v", released)
	}
}

func TestRequestStopEndsWithItsContext(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	liveOwner(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	released, err := RequestStop(ctx, root, "project")
	if !errors.Is(err, context.DeadlineExceeded) || released {
		t.Fatalf("RequestStop = released %v, %v; want it to end with its context", released, err)
	}
}
