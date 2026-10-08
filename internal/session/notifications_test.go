package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

// sessionView builds a view over a project directory and two overlay source
// directories, using only the exported API.
func sessionView(t *testing.T) (v *overlayfs.View, project, shared, specific string) {
	t.Helper()
	project = filepath.Join(t.TempDir(), "project")
	shared = filepath.Join(t.TempDir(), "shared")
	specific = filepath.Join(t.TempDir(), "specific")

	if err := os.MkdirAll(project, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shared, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(specific, 0755); err != nil {
		t.Fatal(err)
	}

	projectRoot, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	sharedRoot, err := os.OpenRoot(shared)
	if err != nil {
		t.Fatal(err)
	}
	specificRoot, err := os.OpenRoot(specific)
	if err != nil {
		t.Fatal(err)
	}

	v = &overlayfs.View{}
	v.AddProject(projectRoot)
	v.AddOverlay(sharedRoot, "**/*")
	v.AddOverlay(specificRoot, "**/*")
	t.Cleanup(func() { v.Close() })
	return v, project, shared, specific
}

func TestNativeNotifications(t *testing.T) {
	v, base, shared, _ := sessionView(t)
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
	cases := []struct {
		event fsnotify.Event
		want  bool
	}{
		{fsnotify.Event{Name: filepath.Join(git, "refs/heads/main"), Op: fsnotify.Write}, false},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees"), Op: fsnotify.Create}, true},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees/one/gitdir"), Op: fsnotify.Write}, true},
		{fsnotify.Event{Name: filepath.Join(git, "worktrees/one/locked"), Op: fsnotify.Remove}, true},
		{fsnotify.Event{Name: filepath.Join(source, "file"), Op: fsnotify.Write}, true},
		{fsnotify.Event{Name: filepath.Join(source, "file"), Op: fsnotify.Chmod}, false},
		{fsnotify.Event{Name: "/unrelated", Op: fsnotify.Create}, false},
	}
	for _, tt := range cases {
		if got := n.relevant(tt.event); got != tt.want {
			t.Errorf("relevant(%v)=%v, want %v", tt.event, got, tt.want)
		}
	}
	scratch.Write(t, filepath.Join(shared, "added.md"), "live")
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-n.watcher.Events:
			if n.relevant(event) {
				return
			}
		case err := <-n.watcher.Errors:
			t.Fatal(err)
		case <-timer.C:
			t.Fatal("native source notification not delivered")
		}
	}
}

func TestNotificationErrors(t *testing.T) {
	cases := []string{"closed source", "corrupt git", "closed backing"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			v, base, _, _ := sessionView(t)
			switch name {
			case "closed source":
				if err := v.Overlays()[0].Close(); err != nil {
					t.Fatal(err)
				}
			case "corrupt git":
				scratch.Write(t, filepath.Join(base, ".git"), "invalid")
			case "closed backing":
				if err := v.Project().Close(); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := newNotifications(context.Background(), mountPlan{target: base, view: v}, true); err == nil {
				t.Fatalf("invalid backing handles accepted (closing them: %v)", n.close())
			}
		})
	}
}

// A watched directory can be deleted before the watcher has processed the
// news. The kernel has dropped its watch by then, so dropping it again fails,
// and that must not stop the session.
func TestSyncToleratesWatchesTheKernelAlreadyDropped(t *testing.T) {
	v, base, _, _ := sessionView(t)
	scratch.GitRepo(t, base)
	addWorktree(t, base, "linked", filepath.Join(t.TempDir(), "linked"))
	n, err := newNotifications(context.Background(), mountPlan{target: base, view: v}, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := n.close(); err != nil {
			t.Error(err)
		}
	})

	// Nothing reads the watcher's events here, so it still lists the watch.
	if err := os.RemoveAll(filepath.Join(base, ".git", "worktrees", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := n.sync(); err != nil {
		t.Fatalf("sync after a watched directory was deleted: %v", err)
	}
}
