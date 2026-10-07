// Package subprocess runs external programs on behalf of a context.
package subprocess

import (
	"context"
	"os/exec"
)

// Output runs cmd, which must have been created for ctx, and returns its
// standard output.
func Output(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	data, err := cmd.Output()
	return data, interrupted(ctx, err)
}

// CombinedOutput runs cmd, which must have been created for ctx, and returns
// its standard output and standard error together.
func CombinedOutput(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	data, err := cmd.CombinedOutput()
	return data, interrupted(ctx, err)
}

// interrupted reports the context's error for a command that failed after its
// context ended. Such a command was killed, and os/exec reports the signal
// that killed it, which says nothing about why it was stopped.
func interrupted(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
