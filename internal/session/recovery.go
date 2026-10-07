package session

import (
	"context"
	"fmt"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitexclude"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/registry"
)

func stopRegistered(ctx context.Context, root, project string) error {
	released, err := registry.RequestStop(ctx, root, project)
	if err != nil || released {
		return err
	}
	return recoverRegistry(ctx, root, project)
}

func recoverRegistry(ctx context.Context, root, project string) error {
	r, err := registry.Acquire(root, project)
	if err != nil {
		return err
	}
	defer r.Unlock()
	state, err := registry.Read(root, project)
	if err != nil {
		return err
	}
	r.Adopt(state)
	for _, mount := range state.Mounts {
		if err := unmountOverlay(ctx, mount.Target); err != nil {
			return err
		}
	}
	for _, mount := range state.Mounts {
		if mount.Exclude != "" && mount.Block != "" {
			if err := gitexclude.Restore(mount.Exclude, []byte(mount.Block)); err != nil {
				return fmt.Errorf("recover Git exclusions: %w", err)
			}
		}
		if err := r.Forget(mount.Target); err != nil {
			return err
		}
	}
	return r.Discard()
}
