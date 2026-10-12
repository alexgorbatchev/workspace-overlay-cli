package overlayfs

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestProcessCommReadBudget(t *testing.T) {
	pid := uint32(os.Getpid())
	want := readProcessComm(pid)
	if want == "" {
		t.Fatal("cannot read this process's name")
	}
	allocs := testing.AllocsPerRun(100, func() {
		if got := readProcessComm(pid); got != want {
			t.Fatalf("comm = %q, want %q", got, want)
		}
	})
	if allocs > 5 {
		t.Errorf("reading a process name allocated %.0f times, want <=5", allocs)
	}
}

func TestProcessCommChangesAreImmediatelyVisible(t *testing.T) {
	pid := uint32(os.Getpid())
	name := fmt.Sprintf("/proc/%d/comm", pid)
	original, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(name, original, 0644); err != nil {
			t.Error(err)
		}
	})
	for _, comm := range []string{"editor", "git", "git-remote-http", "123456789012345", " editor ", "editor"} {
		if err := os.WriteFile(name, []byte(comm), 0644); err != nil {
			t.Fatal(err)
		}
		ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: pid})
		wantGit := comm == "git" || comm == "git-remote-http"
		if got := isGitCaller(ctx); got != wantGit {
			t.Errorf("comm %q: Git = %v, want %v", comm, got, wantGit)
		}
		want := comm
		if comm == " editor " {
			want = "editor"
		}
		if got := readProcessComm(pid); got != want {
			t.Errorf("comm = %q, want %q", got, want)
		}
	}
}

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
