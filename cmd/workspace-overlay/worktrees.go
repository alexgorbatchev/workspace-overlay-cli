package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type mountPlan struct {
	target  string
	sources []overlaySource
	view    *view
}

type mountSelection struct {
	root, project, target string
	sources               []overlaySource
	replace, worktrees    bool
}

func projectGit(ctx context.Context, target string, args ...string) *exec.Cmd {
	command := []string{"-C", target, "--git-dir=" + filepath.Join(target, ".git")}
	return exec.CommandContext(ctx, "git", append(command, args...)...)
}

func projectTargets(ctx context.Context, root, project string, worktrees bool) ([]string, error) {
	target, err := projectPath(root, project)
	if err != nil {
		return nil, err
	}
	return discoverWorktrees(ctx, target, worktrees)
}

func discoverWorktrees(ctx context.Context, target string, worktrees bool) ([]string, error) {
	targets := []string{target}
	if !worktrees {
		return targets, nil
	}
	if _, err := os.Lstat(filepath.Join(target, ".git")); errors.Is(err, os.ErrNotExist) {
		return targets, nil
	} else if err != nil {
		return nil, err
	}
	cmd := projectGit(ctx, target, "worktree", "list", "--porcelain", "-z")
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("discover worktrees: %w", err)
	}
	seen := map[string]bool{target: true}
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
		if name == "" || unavailable || seen[name] {
			continue
		}
		seen[name] = true
		targets = append(targets, name)
	}
	return targets, nil
}

func mountProjects(ctx context.Context, selection mountSelection) error {
	if selection.target == "" {
		target, err := projectPath(selection.root, selection.project)
		if err != nil {
			return err
		}
		selection.target = target
	}
	if selection.replace {
		if err := stopRegistered(ctx, selection.root, selection.project); err != nil {
			return err
		}
	}
	targets, err := discoverWorktrees(ctx, selection.target, selection.worktrees)
	if err != nil {
		return err
	}
	for _, target := range targets {
		kind, err := mountedType(ctx, target)
		if err != nil {
			return err
		}
		if kind != "" && (kind != overlayType || !selection.replace) {
			return fmt.Errorf("%s is mounted as %s; overlay replacement requires --replace", displayPath(target), kind)
		}
	}
	if selection.replace {
		for _, target := range targets {
			if err := unmountOverlay(ctx, target); err != nil {
				return err
			}
		}
	}
	return runProject(ctx, selection, targets)
}

func mountSelections(ctx context.Context, selections []mountSelection) error {
	// Reject overlapping project/worktree sets before any filesystem is changed.
	var targets []string
	for _, selection := range selections {
		names, err := discoverWorktrees(ctx, selection.target, selection.worktrees)
		if err != nil {
			return err
		}
		for _, name := range names {
			name, err := filepath.EvalSymlinks(name)
			if err != nil {
				return fmt.Errorf("open layer: %w", err)
			}
			for _, existing := range targets {
				if containsPath(existing, name) || containsPath(name, existing) {
					return fmt.Errorf("mount targets overlap: %s and %s", displayPath(existing), displayPath(name))
				}
			}
			targets = append(targets, name)
		}
	}
	for _, selection := range selections {
		for _, source := range selection.sources {
			resolved, err := filepath.EvalSymlinks(source.path)
			if err != nil {
				return fmt.Errorf("open layer %s: %w", displayPath(source.path), err)
			}
			for _, target := range targets {
				if containsPath(target, resolved) {
					return fmt.Errorf("overlay source %s lies inside mount target %s", displayPath(source.path), displayPath(target))
				}
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
		if err := <-finished; err != nil {
			result = errors.Join(result, err)
			cancel()
		}
	}
	return result
}

func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func manageProjects(ctx context.Context, out io.Writer, action string, selection mountSelection) error {
	state, err := readRegistry(selection.root, selection.project)
	if err != nil {
		return err
	}
	if action == "unmount" {
		if err := stopRegistered(ctx, selection.root, selection.project); err != nil {
			return err
		}
	}
	primary := selection.target
	if primary == "" {
		primary, err = projectPath(selection.root, selection.project)
		if err != nil {
			return err
		}
	}
	targets, err := discoverWorktrees(ctx, primary, selection.worktrees)
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
		if selection.worktrees {
			_, err = fmt.Fprintf(out, "%s\t%s\n", displayPath(target), kind)
		} else {
			_, err = fmt.Fprintln(out, kind)
		}
		result = errors.Join(result, err)
	}
	return result
}
