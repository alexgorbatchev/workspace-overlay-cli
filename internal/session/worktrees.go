package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitrepo"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/registry"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/subprocess"
)

type mountPlan struct {
	target  string
	sources []config.Source
	view    *overlayfs.View
}

// Selection is one configured project to mount, inspect or unmount, with the
// options of this invocation.
type Selection struct {
	Root, Project, Target string
	Sources               []config.Source
	// Replace stops an existing overlay of the project before mounting.
	Replace bool
	// Worktrees includes the project's registered Git worktrees.
	Worktrees bool
	// Suppressed lists worktrees that must stay unmounted.
	Suppressed []string
}

func stopExisting(ctx context.Context, selection Selection) error {
	if err := stopRegistered(ctx, selection.Root, selection.Project); err != nil {
		return err
	}
	kind, err := mountedType(ctx, selection.Target)
	if err != nil {
		return err
	}
	if kind == overlayfs.FilesystemType {
		return unmountOverlay(ctx, selection.Target)
	}
	return nil
}

// discoverWorktrees returns target and its registered worktrees. It looks at
// target through its path, so it is for projects this process does not serve;
// a running session uses projectRunner.targets.
func discoverWorktrees(ctx context.Context, target string, worktrees bool) ([]string, error) {
	if !worktrees {
		return []string{target}, nil
	}
	if _, err := os.Lstat(filepath.Join(target, ".git")); errors.Is(err, os.ErrNotExist) {
		return []string{target}, nil
	} else if err != nil {
		return nil, err
	}
	return listWorktrees(ctx, target)
}

// listWorktrees asks Git for target and the worktrees linked to it.
func listWorktrees(ctx context.Context, target string) ([]string, error) {
	targets := []string{target}
	cmd := gitrepo.Command(ctx, target, "worktree", "list", "--porcelain", "-z")
	data, err := subprocess.Output(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("discover worktrees: %w", err)
	}
	seen := map[string]bool{pathname.Canonical(target): true}
	for _, record := range strings.Split(string(data), "\x00\x00") {
		var name string
		unavailable := false
		for _, field := range strings.Split(record, "\x00") {
			if strings.HasPrefix(field, "worktree ") {
				name = strings.TrimPrefix(field, "worktree ")
			}
			if field == "bare" || field == "prunable" || strings.HasPrefix(field, "prunable ") || field == "locked initializing" {
				unavailable = true
			}
		}
		if name == "" || unavailable {
			continue
		}
		key := pathname.Canonical(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		targets = append(targets, name)
	}
	return targets, nil
}

func mountProjects(ctx context.Context, selection Selection) error {
	if selection.Replace {
		if err := stopExisting(ctx, selection); err != nil {
			return err
		}
	}
	targets, err := discoverWorktrees(ctx, selection.Target, selection.Worktrees)
	if err != nil {
		return err
	}
	targets = slices.DeleteFunc(targets, func(target string) bool { return slices.Contains(selection.Suppressed, target) })
	for _, target := range targets {
		kind, err := mountedType(ctx, target)
		if err != nil {
			return err
		}
		if kind != "" && kind != overlayfs.FilesystemType {
			return fmt.Errorf("%s is mounted as %s; refusing to replace a filesystem that is not an overlay", pathname.Display(target), kind)
		}
		if kind == overlayfs.FilesystemType && !selection.Replace {
			return fmt.Errorf("%s already has an overlay mount; use --replace to restart it", pathname.Display(target))
		}
	}
	if selection.Replace {
		for _, target := range targets {
			if err := unmountOverlay(ctx, target); err != nil {
				return err
			}
		}
	}
	return runProject(ctx, selection, targets)
}

// Mount serves every selection and its worktrees until ctx ends or one project
// fails, which stops the others.
func Mount(ctx context.Context, selections []Selection) error {
	// --replace must clear a previous owner first: its mounts may be dead, and
	// the checks below cannot even stat a dead mount.
	for _, selection := range selections {
		if selection.Replace {
			if err := stopExisting(ctx, selection); err != nil {
				return err
			}
		}
	}

	// Pre-flight validation: one stray worktree must not keep every project from mounting.
	// Resolve all overlay sources once.
	var sources []string
	for _, selection := range selections {
		for _, source := range selection.Sources {
			resolved, err := filepath.EvalSymlinks(source.Path)
			if err != nil {
				return fmt.Errorf("open layer %s: %w", pathname.Display(source.Path), err)
			}
			sources = append(sources, resolved)
		}
	}
	// First pass: validate primaries.
	var accepted []string
	pending := make([][]string, len(selections))
	for i, selection := range selections {
		names, err := discoverWorktrees(ctx, selection.Target, selection.Worktrees)
		if err != nil {
			return err
		}
		if selection.Replace {
			for _, name := range names {
				if err := unmountOverlay(ctx, name); err != nil {
					return err
				}
			}
		}
		primary := names[0]
		resolved, err := filepath.EvalSymlinks(primary)
		if err != nil {
			return fmt.Errorf("open layer: %w", err)
		}
		for _, existing := range accepted {
			if pathname.Contains(existing, resolved) || pathname.Contains(resolved, existing) {
				return fmt.Errorf("mount targets overlap: %s and %s", pathname.Display(existing), pathname.Display(resolved))
			}
		}
		for _, source := range sources {
			if pathname.Contains(resolved, source) {
				return fmt.Errorf("overlay source %s lies inside mount target %s", pathname.Display(source), pathname.Display(resolved))
			}
		}
		accepted = append(accepted, resolved)
		pending[i] = names[1:]
	}
	// Second pass: validate worktrees, skipping unsupported ones.
	for i := range selections {
		for _, name := range pending[i] {
			resolved, err := filepath.EvalSymlinks(name)
			if err != nil {
				return fmt.Errorf("open layer: %w", err)
			}
			reason := unsupportedWorktree(resolved, accepted, sources)
			if reason != "" {
				log.Printf("Skipping worktree %s: %s", pathname.Display(name), reason)
				selections[i].Suppressed = append(selections[i].Suppressed, name)
			} else {
				accepted = append(accepted, resolved)
			}
		}
	}
	group, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, len(selections))
	for _, selection := range selections {
		go func() { finished <- mountProjects(group, selection) }()
	}
	var result error
	for range selections {
		err := <-finished
		// The first failure tells the other projects to stop. That their work
		// was interrupted is a consequence, not a failure to report.
		if err == nil || stopped(group, err) {
			continue
		}
		result = errors.Join(result, err)
		cancel()
	}
	return result
}

// stopped reports whether err only says that ctx ended. That is how a project
// is told to stop, so it is not a failure of the project.
func stopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, ctx.Err())
}

// unsupportedWorktree explains why a linked worktree cannot be mounted
// beside the accepted targets, or returns "" when it can.
func unsupportedWorktree(resolved string, accepted, sources []string) string {
	for _, existing := range accepted {
		if pathname.Contains(existing, resolved) || pathname.Contains(resolved, existing) {
			return "overlaps mount target " + pathname.Display(existing)
		}
	}
	for _, source := range sources {
		if pathname.Contains(resolved, source) {
			return "contains overlay source " + pathname.Display(source)
		}
	}
	return ""
}

// Status writes one line per target of the selection: its path, a tab, and its
// filesystem type, or "unmounted".
func Status(ctx context.Context, out io.Writer, selection Selection) error {
	return manageProjects(ctx, out, "status", selection)
}

// Unmount stops the selection's owner and removes its mounts.
func Unmount(ctx context.Context, selection Selection) error {
	return manageProjects(ctx, io.Discard, "unmount", selection)
}

func manageProjects(ctx context.Context, out io.Writer, action string, selection Selection) error {
	state, err := registry.Read(selection.Root, selection.Project)
	if err != nil {
		return err
	}
	if action == "unmount" {
		if err := stopRegistered(ctx, selection.Root, selection.Project); err != nil {
			return err
		}
	}
	targets, err := discoverWorktrees(ctx, selection.Target, selection.Worktrees)
	if err != nil && len(state.Mounts) == 0 {
		return err
	}
	seen := make(map[string]bool)
	for _, target := range targets {
		seen[target] = true
	}
	for _, mount := range state.Mounts {
		if !seen[mount.Target] {
			targets = append(targets, mount.Target)
			seen[mount.Target] = true
		}
	}
	var result error
	for _, target := range targets {
		if action == "unmount" {
			result = errors.Join(result, unmountOverlay(ctx, target))
			continue
		}
		kind, err := mountedType(ctx, target)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if kind == "" {
			kind = "unmounted"
		}
		_, err = fmt.Fprintf(out, "%s\t%s\n", pathname.Display(target), kind)
		result = errors.Join(result, err)
	}
	return result
}

// Selections pairs each configured project with the mount options of one
// invocation.
func Selections(cfg *config.Configuration, project string, replace, worktrees bool) ([]Selection, error) {
	selections, err := cfg.Selections(project)
	if err != nil {
		return nil, err
	}
	var mounts []Selection
	for _, s := range selections {
		mounts = append(mounts, Selection{Root: s.Root, Project: s.Project, Target: s.Target, Sources: s.Sources, Replace: replace, Worktrees: worktrees})
	}
	return mounts, nil
}
