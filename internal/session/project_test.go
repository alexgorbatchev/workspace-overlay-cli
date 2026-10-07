package session

import (
	"bytes"
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

// testProjectRunner returns a runner for a project without Git metadata and
// the plan prepared for that project, which no mount serves yet.
func testProjectRunner(t *testing.T) (*projectRunner, mountPlan) {
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
	p := &projectRunner{selection: selection, records: records, notify: n, active: map[string]*runningMount{}, suppressed: map[string]string{}, events: make(chan *runningMount, 1), project: plan.view.Project()}
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
	return p, plan
}

func TestProjectWatcherClosure(t *testing.T) {
	p, _ := testProjectRunner(t)
	if err := p.notify.watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.loop(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed watcher: %v", err)
	}
}

func TestProjectWatcherOverflow(t *testing.T) {
	p, _ := testProjectRunner(t)
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
			p, _ := testProjectRunner(t)
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
				if _, suppressed := p.suppressed["/proc"]; !suppressed {
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

// A project that fails to start after some of its targets were prepared
// leaves no trace of them: no Git exclusions and no registry record.
func TestFailedStartDiscardsPreparedPlans(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	source := filepath.Join(root, "source")
	scratch.Write(t, filepath.Join(source, "overlay-only.md"), "overlay")
	exclude := filepath.Join(project, ".git", "info", "exclude")
	original := scratch.Read(t, exclude)
	selection := Selection{Root: root, Project: "project", Target: project, Sources: []config.Source{{Name: "source", Path: source, Glob: "**/*"}}}

	// The second target does not exist, so the start fails after the first
	// target was prepared and its exclusions were written.
	if err := runProject(context.Background(), selection, []string{project, filepath.Join(root, "missing")}); err == nil {
		t.Fatal("missing target accepted")
	}

	if got := scratch.Read(t, exclude); !bytes.Equal(got, original) {
		t.Errorf("exclude file after a failed start:\n%s\nwant it restored to:\n%s", got, original)
	}
	state, err := registry.Read(root, "project")
	if err != nil || state.Version != 0 {
		t.Fatalf("registry after a failed start: %+v, %v; want no record", state, err)
	}
}

// The session decides whether a project it serves has Git metadata from the
// directory it opened before mounting, never from the project's path: a
// request to its own mount could not be answered if the process were killed
// meanwhile. The test stands a different directory at the path, the way a
// mount does, and expects the session not to consult it.
func TestReconcileInspectsServedProjectThroughItsBackingDirectory(t *testing.T) {
	p, plan := testProjectRunner(t)
	project := p.selection.Target
	p.selection.Worktrees = true
	p.active[project] = &runningMount{plan: plan}
	if err := os.Rename(project, project+".backing"); err != nil {
		t.Fatal(err)
	}
	scratch.GitRepo(t, project)
	linked := filepath.Join(p.selection.Root, "linked")
	addWorktree(t, project, "linked", linked)
	t.Cleanup(func() {
		if mount := p.active[linked]; mount != nil {
			mount.cancel()
			<-mount.done
		}
	})

	if err := p.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	if p.active[linked] != nil {
		t.Fatal("the session looked through the project path and mounted a worktree its backing directory does not have")
	}
}
