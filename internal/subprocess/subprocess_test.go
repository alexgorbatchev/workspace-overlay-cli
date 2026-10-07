package subprocess

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMain(m *testing.M) {
	scratch.Main(m)
}

func TestInterruptedCommandReportsItsContext(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *exec.Cmd) ([]byte, error)
	}{
		{"Output", Output},
		{"CombinedOutput", CombinedOutput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := tt.run(ctx, exec.CommandContext(ctx, "sleep", "30"))
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("interrupted command = %v, want the context's error", err)
			}
		})
	}
}

func TestCompletedCommandKeepsItsResult(t *testing.T) {
	ctx := context.Background()
	out, err := Output(ctx, exec.CommandContext(ctx, "sh", "-c", "echo out; echo err >&2"))
	if err != nil || string(out) != "out\n" {
		t.Fatalf("Output = %q, %v; want standard output only", out, err)
	}

	out, err = CombinedOutput(ctx, exec.CommandContext(ctx, "sh", "-c", "echo out; echo err >&2; exit 3"))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("failed command = %v, want its exit status 3", err)
	}
	if string(out) != "out\nerr\n" {
		t.Fatalf("CombinedOutput = %q, want both streams", out)
	}
}
