package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fsnotify/fsnotify"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/registry"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func testProjectRunner(t *testing.T) *projectRunner {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0755); err != nil {
		t.Fatal(err)
	}
	records, err := registry.Open(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	selection := Selection{Root: root, Project: "project", Target: project}
	plan, err := preparePlan(context.Background(), selection, project, nil, records)
	if err != nil {
		t.Fatal(err)
	}
	n, err := newNotifications(context.Background(), plan, false)
	if err != nil {
		t.Fatal(err)
	}
	p := &projectRunner{selection: selection, records: records, notify: n, active: map[string]*runningMount{}, suppressed: map[string]bool{}, events: make(chan *runningMount, 1)}
	t.Cleanup(func() {
		if err := n.close(); err != nil {
			t.Log(err)
		}
		plan.view.Close()
		if err := records.Forget(project); err != nil {
			t.Error(err)
		}
		if err := records.Close(); err != nil {
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
	p.selection.Worktrees = true
	scratch.Write(t, filepath.Join(p.selection.Target, ".git"), "invalid")
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
				p.selection.Worktrees = true
				scratch.Write(t, filepath.Join(p.selection.Target, ".git"), "invalid")
			case "new foreign mount":
				p.selection.Target = "/proc"
			case "missing working directory":
				p.selection.Target = filepath.Join(p.selection.Root, "missing")
			}
			err := p.reconcile(context.Background())
			if name == "missing working directory" {
				if err != nil {
					t.Fatal(err)
				}
			} else if name == "new foreign mount" {
				if err != nil {
					t.Fatal(err)
				}
				if !p.suppressed["/proc"] {
					t.Fatal("foreign mount was not suppressed")
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
	scratch.GitRepo(t, project)
	source := filepath.Join(root, "source")
	scratch.Write(t, filepath.Join(source, "AGENTS.md"), "overlay")
	selection := Selection{Root: root, Project: "project", Target: project, Sources: []config.Source{{Path: source, Glob: "["}}}
	if err := mountProjects(context.Background(), selection); err == nil {
		t.Fatal("invalid layer glob accepted")
	}
	state, err := registry.Read(root, "project")
	if err != nil || len(state.Mounts) != 0 {
		t.Fatalf("failed mount retained records: %+v %v", state, err)
	}
}
