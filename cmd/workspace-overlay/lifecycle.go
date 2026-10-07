package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const overlayType = "fuse.workspace-overlay"

func ensureWorktreeIsolation(ctx context.Context, target string, sources []overlaySource) error {
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return err
	}
	for _, source := range sources {
		name, err := filepath.EvalSymlinks(source.path)
		if err != nil {
			return err
		}
		if containsPath(resolved, name) {
			return fmt.Errorf("new worktree contains an overlay source: %s", displayPath(target))
		}
	}
	cmd := exec.CommandContext(ctx, "findmnt", "--json", "--list", "--types", overlayType, "--output", "TARGET")
	data, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 && len(data) == 0 && len(exit.Stderr) == 0 {
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
		if containsPath(mount.Target, resolved) || containsPath(resolved, mount.Target) {
			return fmt.Errorf("new worktree overlaps an active mount: %s", displayPath(target))
		}
	}
	return nil
}

func mountedType(ctx context.Context, target string) (string, error) {
	target, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "findmnt", "--raw", "--noheadings", "--output", "FSTYPE", "--mountpoint", target)
	var diagnostics strings.Builder
	cmd.Stderr = &diagnostics
	data, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 && len(data) == 0 && diagnostics.Len() == 0 {
			return "", nil
		}
		return "", fmt.Errorf("inspect mount %s: %w: %s", target, err, strings.TrimSpace(diagnostics.String()))
	}
	return strings.TrimSpace(string(data)), nil
}

func unmountOverlay(ctx context.Context, target string) error {
	kind, err := mountedType(ctx, target)
	if err != nil {
		return err
	}
	if kind == "" {
		return nil
	}
	if kind != overlayType {
		return fmt.Errorf("refusing to unmount %s: filesystem is %s", target, kind)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	data, err := exec.CommandContext(ctx, "fusermount3", "-u", "--", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("unmount %s: %w: %s", target, err, strings.TrimSpace(string(data)))
	}
	return nil
}
