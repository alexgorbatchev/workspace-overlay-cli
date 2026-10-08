package session

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

func serve(ctx context.Context, plan mountPlan) error {
	server, err := plan.view.Mount(plan.target)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	paths := []string{pathname.Display(plan.target)}
	for _, source := range plan.sources {
		paths = append(paths, pathname.Display(source.Path))
	}
	fmt.Fprintf(os.Stderr, "Mounted %s; Ctrl-C to unmount.\n", strings.Join(paths, " + "))
	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// The stop request ended ctx, and the unmount still has work to do.
		if err := release(context.WithoutCancel(ctx), plan.target, plan.confirm, server.Unmount); err != nil {
			return fmt.Errorf("unmount overlay: %w", err)
		}
		<-done
		return nil
	}
}
