package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fsnotify/fsnotify"
)

func testProjectRunner(t *testing.T) *projectRunner {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	r, err := openRegistry(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	selection := mountSelection{root: root, project: "project", target: project}
	plan, err := preparePlan(context.Background(), selection, project, nil, r)
	if err != nil {
		t.Fatal(err)
	}
	n, err := newNotifications(context.Background(), plan, false)
	if err != nil {
		t.Fatal(err)
	}
	p := &projectRunner{selection: selection, registry: r, notify: n, active: map[string]*runningMount{}, suppressed: map[string]bool{}, events: make(chan *runningMount, 1)}
	t.Cleanup(func() {
		if err := n.close(); err != nil {
			t.Log(err)
		}
		plan.view.close()
		if err := r.forget(project); err != nil {
			t.Error(err)
		}
		if err := r.close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func TestProjectWatcherClosure(t *testing.T) {
	p := testProjectRunner(t)
	if err := p.notify.watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.loop(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed watcher: %v", err)
	}
}

func TestProjectWatcherOverflow(t *testing.T) {
	p := testProjectRunner(t)
	// The real fsnotify channel accepts the same overflow error delivered by inotify.
	go func() { p.notify.watcher.Errors <- fsnotify.ErrEventOverflow }()
	p.selection.worktrees = true
	writeFixture(t, filepath.Join(p.selection.target, ".git"), "invalid")
	if err := p.loop(context.Background()); err == nil || !strings.Contains(err.Error(), "discover worktrees") {
		t.Fatalf("overflow rescan: %v", err)
	}
}

func TestProjectReconcileErrors(t *testing.T) {
	cases := []string{"corrupt metadata", "new foreign mount", "missing working directory"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			p := testProjectRunner(t)
			switch name {
			case "corrupt metadata":
				p.selection.worktrees = true
				writeFixture(t, filepath.Join(p.selection.target, ".git"), "invalid")
			case "new foreign mount":
				p.selection.target = "/proc"
			case "missing working directory":
				p.selection.target = filepath.Join(p.selection.root, "missing")
			}
			err := p.reconcile(context.Background())
			if name == "missing working directory" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid discovery state accepted")
			}
		})
	}
}

func TestFailedPlanRestoresExclusions(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	initGitRepoWithCommit(t, project)
	source := filepath.Join(root, "source")
	writeFixture(t, filepath.Join(source, "AGENTS.md"), "overlay")
	selection := mountSelection{root: root, project: "project", target: project, sources: []overlaySource{{path: source, glob: "["}}}
	if err := mountProjects(context.Background(), selection); err == nil {
		t.Fatal("invalid layer glob accepted")
	}
	state, err := readRegistry(root, "project")
	if err != nil || len(state.Mounts) != 0 {
		t.Fatalf("failed mount retained records: %+v %v", state, err)
	}
}
