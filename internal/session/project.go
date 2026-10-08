package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitexclude"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitrepo"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/registry"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/subprocess"
)

const debounceDelay = 200 * time.Millisecond
const reconcileInterval = 30 * time.Second

type runningMount struct {
	plan   mountPlan
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

type projectRunner struct {
	selection Selection
	records   *registry.Registry
	notify    *notifications
	active    map[string]*runningMount
	// suppressed holds the worktrees that must stay unmounted, each with the
	// name of its Git registration, or "" when that is unknown.
	suppressed map[string]string
	events     chan *runningMount
	primary    *gitexclude.File
	// project is the project's own directory, opened before it was mounted.
	project *os.Root
}

// discard releases a plan that is not being served: it removes the plan's Git
// exclusions, forgets its record once those are gone, and closes its handles.
func (plan mountPlan) discard(records *registry.Registry) error {
	err := plan.view.Exclude().Close()
	if err == nil {
		err = records.Forget(plan.target)
	}
	plan.view.Close()
	return err
}

func preparePlan(ctx context.Context, selection Selection, target string, template *gitexclude.File, records *registry.Registry) (plan mountPlan, result error) {
	plan = mountPlan{target: target, sources: selection.Sources, view: &overlayfs.View{}}
	defer func() {
		if result != nil {
			result = errors.Join(result, plan.discard(records))
		}
	}()
	backing, err := os.OpenRoot(target)
	if err != nil {
		return plan, fmt.Errorf("open layer %s: %w", pathname.Display(target), err)
	}
	plan.view.AddProject(backing)
	if len(selection.Markers) > 0 {
		var rules []overlayfs.MarkerRule
		for _, m := range selection.Markers {
			rules = append(rules, overlayfs.MarkerRule{Glob: m.Glob, Start: m.Start, End: m.End})
		}
		plan.view.SetMarkers(rules)
	}
	for _, source := range selection.Sources {
		root, err := os.OpenRoot(source.Path)
		if err != nil {
			return plan, fmt.Errorf("open layer %s: %w", pathname.Display(source.Path), err)
		}
		plan.view.AddOverlay(root, source.Name, source.Glob)
	}
	var exclude *gitexclude.File
	if template != nil {
		exclude, err = template.Clone()
	} else {
		exclude, err = gitexclude.Open(ctx, target)
	}
	if err != nil {
		return plan, err
	}
	plan.view.SetExclude(exclude)
	var excludePath string
	if g := plan.view.Exclude(); g != nil {
		excludePath = g.Path()
		g.OnUpdate = func(block []byte) error { return records.Record(target, excludePath, block) }
	}
	if err := records.Record(target, excludePath, nil); err != nil {
		return plan, err
	}
	if err := plan.view.RefreshPaths(); err != nil {
		return plan, err
	}
	return plan, nil
}

func runProject(ctx context.Context, selection Selection, targets []string) (result error) {
	records, err := registry.Open(selection.Root, selection.Project)
	if err != nil {
		return err
	}
	p := &projectRunner{selection: selection, records: records, active: make(map[string]*runningMount), suppressed: make(map[string]string), events: make(chan *runningMount)}
	for _, target := range selection.Suppressed {
		p.suppress(ctx, target)
	}
	group, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		cancel()
		for _, mount := range p.active {
			mount.cancel()
		}
		for _, mount := range p.active {
			<-mount.done
			result = errors.Join(result, mount.err)
		}
		if p.notify != nil {
			result = errors.Join(result, p.notify.close())
		}
		result = errors.Join(result, records.Close())
	}()
	var plans []mountPlan
	defer func() {
		for _, plan := range plans {
			result = errors.Join(result, plan.discard(records))
		}
	}()
	// Runs before the cleanup above adds its own errors to the result.
	defer func() {
		if stopped(ctx, result) {
			result = nil
		}
	}()
	// Git common directories can lie underneath another target in this set.
	for _, target := range targets {
		plan, err := preparePlan(group, selection, target, nil, records)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	p.primary = plans[0].view.Exclude()
	p.project = plans[0].view.Project()
	p.notify, err = newNotifications(group, plans[0], selection.Worktrees)
	if err != nil {
		return err
	}
	p.notify.control = records.Dir()
	if err := p.notify.sync(); err != nil {
		return err
	}
	for _, plan := range plans {
		p.start(group, plan)
	}
	plans = nil // The workers now own every backing handle.
	return p.loop(group)
}

// skip leaves one worktree unmounted without stopping the project. It is
// retried only after Git unregisters and registers it again.
func (p *projectRunner) skip(ctx context.Context, target string, reason error) {
	log.Printf("Skipping worktree %s: %v", pathname.Display(target), reason)
	p.suppress(ctx, target)
}

// suppress keeps target unmounted for as long as its Git registration lasts.
func (p *projectRunner) suppress(ctx context.Context, target string) {
	p.suppressed[target] = registration(ctx, target)
}

// release lets the worktree registered under name be mounted again; an empty
// name stands for every registration. The registration ended, so a worktree
// now found at the same path is a new one, even if Git registered it again
// before the next listing could show it missing.
func (p *projectRunner) release(name string) {
	for target, registered := range p.suppressed {
		if registered != "" && (name == "" || name == registered) {
			delete(p.suppressed, target)
		}
	}
}

// registration returns the name under which Git registered the linked
// worktree at target, or "" when target is not one or Git cannot tell.
func registration(ctx context.Context, target string) string {
	data, err := subprocess.Output(ctx, gitrepo.Command(ctx, target, "rev-parse", "--absolute-git-dir"))
	if err != nil {
		return ""
	}
	metadata := strings.TrimSpace(string(data))
	if filepath.Base(filepath.Dir(metadata)) != "worktrees" {
		return ""
	}
	return filepath.Base(metadata)
}

func (p *projectRunner) start(ctx context.Context, plan mountPlan) {
	child, cancel := context.WithCancel(ctx)
	mount := &runningMount{plan: plan, cancel: cancel, done: make(chan struct{})}
	p.active[plan.target] = mount
	go func() {
		mount.err = serve(child, plan)
		mount.err = errors.Join(mount.err, plan.view.Exclude().Close())
		plan.view.Close()
		if mount.err == nil {
			mount.err = p.records.Forget(plan.target)
		}
		close(mount.done)
		select {
		case p.events <- mount:
		case <-ctx.Done():
		}
	}()
}

// targets lists the project and its registered worktrees. This process
// serves the project's mount, so it looks for Git metadata in the directory
// it opened before mounting: a process that is killed while it waits on its
// own mount never exits.
func (p *projectRunner) targets(ctx context.Context) ([]string, error) {
	if !p.selection.Worktrees {
		return []string{p.selection.Target}, nil
	}
	if _, err := p.project.Lstat(".git"); errors.Is(err, os.ErrNotExist) {
		return []string{p.selection.Target}, nil
	} else if err != nil {
		return nil, err
	}
	return listWorktrees(ctx, p.selection.Target)
}

func (p *projectRunner) reconcile(ctx context.Context) error {
	targets, err := p.targets(ctx)
	if err != nil {
		return err
	}
	desired := make(map[string]bool)
	for _, target := range targets {
		desired[target] = true
	}
	for target := range p.suppressed {
		if !desired[target] {
			delete(p.suppressed, target)
		}
	}
	for target, mount := range p.active {
		if !desired[target] {
			mount.cancel()
			<-mount.done
			delete(p.active, target)
			if mount.err != nil {
				return mount.err
			}
			log.Printf("Unmounted unregistered worktree %s", pathname.Display(target))
		} else if err := mount.plan.view.RefreshPaths(); err != nil {
			return err
		}
	}
	for _, target := range targets {
		if _, suppressed := p.suppressed[target]; suppressed || p.active[target] != nil {
			continue
		}
		// The candidate may lie inside a mount this process serves, which it
		// must not touch, so another process says where the candidate lives
		// before anything here looks at it.
		holder, err := holdingType(ctx, target)
		if err != nil {
			return err
		}
		if holder == "" {
			// Git registration can precede creation of the working directory.
			continue
		}
		if holder == overlayfs.FilesystemType {
			p.skip(ctx, target, fmt.Errorf("%w: %s overlaps an active mount", errUnsupportedWorktree, pathname.Display(target)))
			continue
		}
		resolved, err := filepath.EvalSymlinks(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := ensureWorktreeIsolation(ctx, resolved, p.selection.Sources); err != nil {
			if errors.Is(err, errUnsupportedWorktree) {
				p.skip(ctx, target, err)
				continue
			}
			return err
		}
		kind, err := mountedType(ctx, target)
		if err != nil {
			return err
		}
		if kind != "" {
			p.skip(ctx, target, fmt.Errorf("%w: already mounted as %s", errUnsupportedWorktree, kind))
			continue
		}
		plan, err := preparePlan(ctx, p.selection, target, p.primary, p.records)
		if err != nil {
			// Git registration can precede creation of the working directory.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		p.start(ctx, plan)
	}
	return p.notify.sync()
}

func (p *projectRunner) loop(ctx context.Context) error {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	timer := time.NewTimer(debounceDelay)
	defer timer.Stop()
	var changed <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case mount := <-p.events:
			if p.active[mount.plan.target] != mount {
				continue
			}
			delete(p.active, mount.plan.target)
			if mount.err != nil {
				return mount.err
			}
			if mount.plan.target == p.selection.Target || len(p.active) == 0 {
				return nil
			}
			p.suppress(ctx, mount.plan.target)
		case event, ok := <-p.notify.watcher.Events:
			if !ok {
				return fmt.Errorf("filesystem watcher closed unexpectedly")
			}
			if event.Name == p.records.StopFile() {
				if _, err := os.Stat(p.records.StopFile()); err == nil {
					return nil
				}
			}
			if name, ended := p.notify.unregistered(event); ended {
				p.release(name)
			}
			if p.notify.relevant(event) {
				if err := p.notify.sync(); err != nil {
					return err
				}
				timer.Reset(debounceDelay)
				changed = timer.C
			}
		case err, ok := <-p.notify.watcher.Errors:
			if !ok {
				return fmt.Errorf("filesystem watcher error channel closed")
			}
			log.Printf("filesystem notifications: %v; reconciling state", err)
			if err := p.reconcile(ctx); err != nil {
				return err
			}
		case <-changed:
			changed = nil
			if err := p.reconcile(ctx); err != nil {
				return err
			}
		case <-ticker.C:
			if _, err := os.Stat(p.records.StopFile()); err == nil {
				return nil
			}
			if err := p.reconcile(ctx); err != nil {
				return err
			}
		}
	}
}
