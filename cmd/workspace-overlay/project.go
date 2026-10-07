package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
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
	selection  mountSelection
	registry   *mountRegistry
	notify     *notifications
	active     map[string]*runningMount
	suppressed map[string]bool
	events     chan *runningMount
	primary    *gitExclude
}

func preparePlan(ctx context.Context, selection mountSelection, target string, template *gitExclude, registry *mountRegistry) (plan mountPlan, result error) {
	plan = mountPlan{target: target, sources: selection.sources, view: &view{}}
	defer func() {
		if result != nil {
			cleanup := plan.view.exclude.close()
			if cleanup == nil {
				cleanup = registry.forget(target)
			}
			result = errors.Join(result, cleanup)
			plan.view.close()
		}
	}()
	backing, err := os.OpenRoot(target)
	if err != nil {
		return plan, fmt.Errorf("open layer %s: %w", displayPath(target), err)
	}
	plan.view.layers = append(plan.view.layers, layer{root: backing})
	for _, source := range selection.sources {
		root, err := os.OpenRoot(source.path)
		if err != nil {
			return plan, fmt.Errorf("open layer %s: %w", displayPath(source.path), err)
		}
		plan.view.layers = append(plan.view.layers, layer{root: root, rules: &pathRules{glob: source.glob}})
	}
	if template != nil {
		plan.view.exclude, err = template.clone()
	} else {
		plan.view.exclude, err = openExclude(ctx, target)
	}
	if err != nil {
		return plan, err
	}
	var exclude string
	if g := plan.view.exclude; g != nil {
		exclude = g.file
		g.onUpdate = func(block []byte) error { return registry.record(target, exclude, block) }
	}
	if err := registry.record(target, exclude, nil); err != nil {
		return plan, err
	}
	if err := plan.view.refreshPaths(); err != nil {
		return plan, err
	}
	return plan, nil
}

func runProject(ctx context.Context, selection mountSelection, targets []string) (result error) {
	registry, err := openRegistry(selection.root, selection.project)
	if err != nil {
		return err
	}
	p := &projectRunner{selection: selection, registry: registry, active: make(map[string]*runningMount), suppressed: make(map[string]bool), events: make(chan *runningMount)}
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
		result = errors.Join(result, registry.close())
	}()
	var plans []mountPlan
	defer func() {
		for _, plan := range plans {
			cleanup := plan.view.exclude.close()
			if cleanup == nil {
				cleanup = registry.forget(plan.target)
			}
			result = errors.Join(result, cleanup)
			plan.view.close()
		}
	}()
	// Git common directories can lie underneath another target in this set.
	for _, target := range targets {
		plan, err := preparePlan(group, selection, target, nil, registry)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	p.primary = plans[0].view.exclude
	p.notify, err = newNotifications(group, plans[0], selection.worktrees)
	if err != nil {
		return err
	}
	p.notify.control = registry.dir
	if err := p.notify.sync(); err != nil {
		return err
	}
	for _, plan := range plans {
		p.start(group, plan)
	}
	plans = nil // The workers now own every backing handle.
	return p.loop(group)
}

func (p *projectRunner) start(ctx context.Context, plan mountPlan) {
	child, cancel := context.WithCancel(ctx)
	mount := &runningMount{plan: plan, cancel: cancel, done: make(chan struct{})}
	p.active[plan.target] = mount
	go func() {
		mount.err = serve(child, plan)
		mount.err = errors.Join(mount.err, plan.view.exclude.close())
		plan.view.close()
		if mount.err == nil {
			mount.err = p.registry.forget(plan.target)
		}
		close(mount.done)
		select {
		case p.events <- mount:
		case <-ctx.Done():
		}
	}()
}

func (p *projectRunner) reconcile(ctx context.Context) error {
	targets, err := discoverWorktrees(ctx, p.selection.target, p.selection.worktrees)
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
			log.Printf("Unmounted unregistered worktree %s", displayPath(target))
		} else if err := mount.plan.view.refreshPaths(); err != nil {
			return err
		}
	}
	for _, target := range targets {
		if p.active[target] != nil || p.suppressed[target] {
			continue
		}
		resolved, err := filepath.EvalSymlinks(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := ensureWorktreeIsolation(ctx, resolved, p.selection.sources); err != nil {
			return err
		}
		kind, err := mountedType(ctx, target)
		if err != nil {
			return err
		}
		if kind != "" {
			return fmt.Errorf("new worktree %s is already mounted as %s", displayPath(target), kind)
		}
		plan, err := preparePlan(ctx, p.selection, target, p.primary, p.registry)
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
			if mount.plan.target == p.selection.target || len(p.active) == 0 {
				return nil
			}
			p.suppressed[mount.plan.target] = true
		case event, ok := <-p.notify.watcher.Events:
			if !ok {
				return fmt.Errorf("filesystem watcher closed unexpectedly")
			}
			if event.Name == p.registry.stop {
				if _, err := os.Stat(p.registry.stop); err == nil {
					return nil
				}
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
			if _, err := os.Stat(p.registry.stop); err == nil {
				return nil
			}
			if err := p.reconcile(ctx); err != nil {
				return err
			}
		}
	}
}
