package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/subprocess"
)

// errUnsupportedWorktree marks a worktree that cannot be mounted without
// harming another mount. Callers skip it instead of stopping the project.
var errUnsupportedWorktree = errors.New("unsupported worktree")

func ensureWorktreeIsolation(ctx context.Context, target string, sources []config.Source) error {
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	for _, source := range sources {
		name, err := filepath.EvalSymlinks(source.Path)
		if err != nil {
			return err
		}
		if pathname.Contains(resolved, name) {
			return fmt.Errorf("%w: %s contains an overlay source", errUnsupportedWorktree, pathname.Display(target))
		}
	}
	cmd := exec.CommandContext(ctx, "findmnt", "--json", "--list", "--types", overlayfs.FilesystemType, "--output", "TARGET")
	data, err := subprocess.Output(ctx, cmd)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(data) == 0 && len(exit.Stderr) == 0 {
			return nil
		}
		return fmt.Errorf("inspect existing overlay mounts: %w", err)
	}
	var result struct {
		Filesystems []struct {
			Target string `json:"target"`
		} `json:"filesystems"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("decode existing overlay mounts: %w", err)
	}
	for _, mount := range result.Filesystems {
		if pathname.Contains(mount.Target, resolved) || pathname.Contains(resolved, mount.Target) {
			return fmt.Errorf("%w: %s overlaps an active mount", errUnsupportedWorktree, pathname.Display(target))
		}
	}
	return nil
}

// mountedType returns the type of the file system mounted exactly at target,
// or "" when nothing is mounted there.
func mountedType(ctx context.Context, target string) (string, error) {
	return findmnt(ctx, "--mountpoint", target)
}

// holdingType returns the type of the file system that holds path, wherever
// in that file system path lies, or "" when path does not exist.
func holdingType(ctx context.Context, path string) (string, error) {
	return findmnt(ctx, "--target", path)
}

// findmnt asks the findmnt command for the file system type selected by
// selector and path. The lookup runs in another process, so a path inside a
// mount that this process serves is never touched from here.
func findmnt(ctx context.Context, selector, path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "findmnt", "--raw", "--noheadings", "--output", "FSTYPE", selector, path)
	var diagnostics strings.Builder
	cmd.Stderr = &diagnostics
	data, err := subprocess.Output(ctx, cmd)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(data) == 0 && diagnostics.Len() == 0 {
			return "", nil
		}
		return "", fmt.Errorf("inspect mount %s: %w: %s", path, err, strings.TrimSpace(diagnostics.String()))
	}
	return strings.TrimSpace(string(data)), nil
}

// unmountOverlay removes the overlay mounted at target. When processes keep
// it busy, confirm decides whether they are stopped; see release.
func unmountOverlay(ctx context.Context, target string, confirm Confirm) error {
	kind, err := mountedType(ctx, target)
	if err != nil {
		return err
	}
	if kind == "" {
		return nil
	}
	if kind != overlayfs.FilesystemType {
		return fmt.Errorf("refusing to unmount %s: filesystem is %s", target, kind)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	return release(ctx, target, confirm, func() error {
		data, err := subprocess.CombinedOutput(ctx, exec.CommandContext(ctx, "fusermount3", "-u", "--", target))
		if err != nil {
			return fmt.Errorf("unmount %s: %w: %s", target, err, strings.TrimSpace(string(data)))
		}
		return nil
	})
}
