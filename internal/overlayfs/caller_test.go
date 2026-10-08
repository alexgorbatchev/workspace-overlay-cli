package overlayfs

import (
	"context"
	"os"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestIsGitCaller(t *testing.T) {
	t.Run("no caller in context", func(t *testing.T) {
		ctx := context.Background()
		if isGitCaller(ctx) {
			t.Error("expected false for context without caller")
		}
	})

	t.Run("caller with pid 0", func(t *testing.T) {
		ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 0})
		if isGitCaller(ctx) {
			t.Error("expected false for pid 0")
		}
	})

	t.Run("caller with non-git process", func(t *testing.T) {
		// Use current process pid (which is "overlayfs.test" or similar, not "git")
		ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: uint32(os.Getpid())})
		if isGitCaller(ctx) {
			t.Errorf("expected false for current process pid %d", os.Getpid())
		}
	})

	t.Run("caller simulated as git", func(t *testing.T) {
		orig := readProcessComm
		defer func() { readProcessComm = orig }()

		readProcessComm = func(pid uint32) string {
			if pid == 99999 {
				return "git"
			}
			return ""
		}

		ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 99999})
		if !isGitCaller(ctx) {
			t.Error("expected true for git caller")
		}
	})

	t.Run("caller simulated as git sub-command", func(t *testing.T) {
		orig := readProcessComm
		defer func() { readProcessComm = orig }()

		readProcessComm = func(pid uint32) string {
			if pid == 99998 {
				return "git-remote-https"
			}
			return ""
		}

		ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 99998})
		if !isGitCaller(ctx) {
			t.Error("expected true for git-remote-https caller")
		}
	})

	t.Run("comm read error", func(t *testing.T) {
		// A non-existent PID returns empty string and isGitCaller is false
		ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: 9999999})
		if isGitCaller(ctx) {
			t.Error("expected false for missing pid")
		}
	})
}
