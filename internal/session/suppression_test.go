package session

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

// A worktree that was unmounted by hand stays unmounted while Git keeps its
// registration, and is mounted again once Git registers it anew, even when
// that follows the end of the old registration at once.
func TestWorktreeRegisteredAgainIsMountedAgain(t *testing.T) {
	tests := []struct {
		name       string
		reregister func(t *testing.T, project, worktree, metadata string)
	}{
		{"moved away and back", func(t *testing.T, project, worktree, metadata string) {
			parked := filepath.Join(project, ".git", "parked-metadata")
			if err := os.Rename(metadata, parked); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(parked, metadata); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed and added", func(t *testing.T, project, worktree, metadata string) {
			for _, args := range [][]string{{"worktree", "remove", worktree}, {"worktree", "add", worktree, "live"}} {
				cmd := exec.Command("git", append([]string{"-C", project}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			scratch.GitRepo(t, project)
			selection := Selection{Root: root, Project: "project", Target: project, Worktrees: true,
				Sources: []config.Source{{Name: "shared", Path: scratch.Mkdir(t, filepath.Join(root, ".ai", "shared")), Glob: "**/*"}}}
			ctx, cancel := context.WithCancel(context.Background())
			finished := make(chan error, 1)
			go func() { finished <- mountProjects(ctx, selection) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-finished:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("session did not stop")
				}
			})
			waitMount(t, project, overlayfs.FilesystemType)
			worktree := filepath.Join(root, ".workspaces", "live")
			addWorktree(t, project, "live", worktree)
			waitMount(t, worktree, overlayfs.FilesystemType)
			out, err := exec.Command("git", "-C", worktree, "rev-parse", "--absolute-git-dir").Output()
			if err != nil {
				t.Fatal(err)
			}
			metadata := strings.TrimSpace(string(out))

			if err := unmountOverlay(context.Background(), worktree); err != nil {
				t.Fatal(err)
			}
			// Two rounds: a mount wrongly started by the first is up by the second.
			awaitReconcile(t, project, root, "first-round")
			awaitReconcile(t, project, root, "second-round")
			waitMount(t, worktree, "")

			tt.reregister(t, project, worktree, metadata)

			waitMount(t, worktree, overlayfs.FilesystemType)
		})
	}
}

func TestUnregisteredRecognizesEndedRegistrations(t *testing.T) {
	v, base, _, _ := sessionView(t)
	scratch.GitRepo(t, base)
	n, err := newNotifications(context.Background(), mountPlan{target: base, view: v}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := n.close(); err != nil {
			t.Error(err)
		}
	})
	git, source := nativePath(n.roots[0].file), nativePath(n.roots[1].file)
	tests := []struct {
		event fsnotify.Event
		name  string
		ended bool
	}{
		{fsnotify.Event{Name: filepath.Join(git, "worktrees/live"), Op: fsnotify.Remove}, "live", true},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees/live"), Op: fsnotify.Rename}, "live", true},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees"), Op: fsnotify.Remove}, "", true},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees/live"), Op: fsnotify.Create}, "", false},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees/live/gitdir"), Op: fsnotify.Remove}, "", false},
		{fsnotify.Event{Name: filepath.Join(git, "refs/heads/live"), Op: fsnotify.Remove}, "", false},
		{fsnotify.Event{Name: filepath.Join(source, "worktrees/live"), Op: fsnotify.Remove}, "", false},
	}
	for _, tt := range tests {
		if name, ended := n.unregistered(tt.event); name != tt.name || ended != tt.ended {
			t.Errorf("unregistered(%v) = %q, %v; want %q, %v", tt.event, name, ended, tt.name, tt.ended)
		}
	}
}

func TestReleaseForgetsOnlyEndedRegistrations(t *testing.T) {
	tests := []struct {
		name string
		want []string
	}{
		{"one", []string{"/foreign", "/two"}},
		{"other", []string{"/foreign", "/one", "/two"}},
		// A path whose registration is unknown waits for a listing without it.
		{"", []string{"/foreign"}},
	}
	for _, tt := range tests {
		p := &projectRunner{suppressed: map[string]string{"/one": "one", "/two": "two", "/foreign": ""}}
		p.release(tt.name)
		if got := slices.Sorted(maps.Keys(p.suppressed)); !slices.Equal(got, tt.want) {
			t.Errorf("after release(%q): %v still suppressed, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRegistrationNamesLinkedWorktreesOnly(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	linked := filepath.Join(root, "elsewhere", "linked")
	addWorktree(t, project, "branch", linked)
	tests := []struct{ target, want string }{
		{linked, "linked"},
		{project, ""},
		{t.TempDir(), ""},
	}
	for _, tt := range tests {
		if got := registration(context.Background(), tt.target); got != tt.want {
			t.Errorf("registration(%s) = %q, want %q", tt.target, got, tt.want)
		}
	}
}
