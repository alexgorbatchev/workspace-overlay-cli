package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

// stalled is an external command made to hang on demand, as it would on a
// slow disk, so a test can stop the session while the command is running.
type stalled struct{ armed, hanging string }

// stall makes program hang whenever its arguments contain match, once armed.
func stall(t *testing.T, program, match string) stalled {
	t.Helper()
	original, err := exec.LookPath(program)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := stalled{armed: filepath.Join(dir, "armed"), hanging: filepath.Join(dir, "hanging")}
	script := fmt.Sprintf("#!/bin/sh\nif [ -e %q ]; then case \"$*\" in *%q*) : > %q; exec sleep 60 ;; esac; fi\nexec %q \"$@\"\n", s.armed, match, s.hanging, original)
	if err := os.WriteFile(filepath.Join(dir, program), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return s
}

func (s stalled) arm(t *testing.T) {
	t.Helper()
	scratch.Write(t, s.armed, "")
}

// wait returns once the command is hanging.
func (s stalled) wait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(s.hanging); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the stalled command never ran")
}

// A project that cannot be mounted reports its own refusal. The other
// projects are told to stop, and their interrupted checks are not failures.
func TestRefusedProjectReportsOnlyItsOwnError(t *testing.T) {
	root := t.TempDir()
	mounted, idle := filepath.Join(root, "mounted"), filepath.Join(root, "idle")
	scratch.GitRepo(t, mounted)
	scratch.GitRepo(t, idle)
	sources := []config.Source{{Name: "shared", Path: scratch.Mkdir(t, filepath.Join(root, ".ai", "shared")), Glob: "**/*"}}
	selections := []Selection{
		{Root: root, Project: "mounted", Target: mounted, Sources: sources},
		{Root: root, Project: "idle", Target: idle, Sources: sources},
	}
	ctx, cancel := context.WithCancel(context.Background())
	owner := make(chan error, 1)
	go func() { owner <- mountProjects(ctx, selections[0]) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-owner:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("owner did not stop")
		}
	})
	waitMount(t, mounted, overlayfs.FilesystemType)
	// The idle project's check hangs, so it is still running when the
	// mounted project is refused.
	stall(t, "findmnt", "--mountpoint "+idle).arm(t)

	err := Mount(context.Background(), selections)

	want := pathname.Display(mounted) + " already has an overlay mount; use --replace to restart it"
	if err == nil || err.Error() != want {
		t.Fatalf("Mount = %v\nwant only: %s", err, want)
	}
}

// Being stopped while a worktree listing is running is a clean stop, as it
// is between listings.
func TestStopDuringReconcileIsNotAFailure(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	selection := Selection{Root: root, Project: "project", Target: project, Worktrees: true,
		Sources: []config.Source{{Name: "shared", Path: scratch.Mkdir(t, filepath.Join(root, ".ai", "shared")), Glob: "**/*"}}}
	listing := stall(t, "git", "worktree list")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- mountProjects(ctx, selection) }()
	waitMount(t, project, overlayfs.FilesystemType)

	listing.arm(t)
	addWorktree(t, project, "late", filepath.Join(root, ".workspaces", "late"))
	listing.wait(t)
	cancel()

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("stopping during a worktree listing = %v, want a clean stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not stop")
	}
	waitMount(t, project, "")
}

// What goes wrong while stopping is still reported: only the interruption
// itself is left out.
func TestStopDuringReconcileStillReportsFailedCleanup(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	shared := filepath.Join(root, ".ai", "shared")
	scratch.Write(t, filepath.Join(shared, "shared.txt"), "shared")
	selection := Selection{Root: root, Project: "project", Target: project, Worktrees: true,
		Sources: []config.Source{{Name: "shared", Path: shared, Glob: "**/*"}}}
	listing := stall(t, "git", "worktree list")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- mountProjects(ctx, selection) }()
	waitMount(t, project, overlayfs.FilesystemType)

	// A managed block that was edited by hand cannot be removed on the way out.
	exclude := filepath.Join(project, ".git", "info", "exclude")
	edited := strings.Replace(string(scratch.Read(t, exclude)), "/shared.txt", "/edited.txt", 1)
	scratch.Write(t, exclude, edited)
	listing.arm(t)
	addWorktree(t, project, "late", filepath.Join(root, ".workspaces", "late"))
	listing.wait(t)
	cancel()

	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "managed Git exclude block changed") {
			t.Fatalf("stop with a failed cleanup = %v, want the cleanup failure", err)
		}
		if strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Fatalf("stop reported its own interruption: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not stop")
	}
	waitMount(t, project, "")
}
