// Package gitrepo runs Git against one checkout.
package gitrepo

import (
	"context"
	"os/exec"
	"path/filepath"
)

// Command builds a Git invocation bound to target's own repository, so a broken or missing .git never falls through to an enclosing repository.
func Command(ctx context.Context, target string, args ...string) *exec.Cmd {
	command := []string{"-C", target, "--git-dir=" + filepath.Join(target, ".git")}
	return exec.CommandContext(ctx, "git", append(command, args...)...)
}
