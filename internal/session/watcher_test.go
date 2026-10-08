package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

// selfReadable prepares an overlay mount for being read by the process that
// serves it, as these tests do with the sessions they run.
//
// The Go runtime registers every file it opens with its poller, and on a FUSE
// mount the first registration asks the file system whether it supports
// polling. Asked from inside the runtime, that question holds a processor the
// runtime believes to be free while the answer has to come from this same
// process, which can lock it up. go-fuse settles the question once per mount
// with plain system calls in Server.WaitMount. The session leaves that call
// out so that its process never touches its own mounts, so it is made here,
// on the file go-fuse serves for this purpose.
func selfReadable(target string) error {
	fd, err := syscall.Open(filepath.Join(target, ".go-fuse-epoll-hack"), syscall.O_RDONLY, 0)
	if err != nil {
		return err
	}
	var files syscall.FdSet
	files.Bits[fd/64] |= 1 << (fd % 64)
	_, err = syscall.Select(fd+1, &files, nil, nil, &syscall.Timeval{})
	return errors.Join(err, syscall.Close(fd))
}

// waitMount returns once target is mounted with the expected file system
// type, or not mounted when expected is empty. An overlay mount is then
// ready to be read by this process.
func waitMount(t *testing.T, target, expected string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		kind, err := mountedType(context.Background(), target)
		if err == nil && kind == expected {
			if expected == overlayfs.FilesystemType {
				if err := selfReadable(target); err != nil {
					t.Fatalf("prepare %s for reading: %v", target, err)
				}
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mount %s did not reach %q", target, expected)
}

// addWorktree registers a linked worktree of project on a new branch.
func addWorktree(t *testing.T, project, branch, path string) {
	t.Helper()
	cmd := exec.Command("git", "-C", project, "worktree", "add", "-b", branch, path, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add worktree %s: %v: %s", branch, err, out)
	}
}

// awaitReconcile returns once the session has acted on everything that
// happened before the call. The session takes one worktree listing per round
// and only that round can mount a worktree registered here, so seeing this
// worktree mounted proves a listing was taken after the call.
func awaitReconcile(t *testing.T, project, root, name string) {
	t.Helper()
	worktree := filepath.Join(root, ".workspaces", name)
	addWorktree(t, project, name, worktree)
	waitMount(t, worktree, overlayfs.FilesystemType)
}

func TestLiveWorktreeDiscovery(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	shared := filepath.Join(root, ".ai", "shared")
	specific := filepath.Join(root, ".ai", "project")
	for _, dir := range []string{shared, specific} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, "shared.txt"), []byte("shared"), 0644); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(project, ".git", "info", "exclude")
	original, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	selection := Selection{Root: root, Project: "project", Target: project, Worktrees: true,
		Sources: []config.Source{{Name: "shared", Path: shared, Glob: "**/*"}, {Name: "project", Path: specific, Glob: "**/*"}}}
	go func() { finished <- mountProjects(ctx, selection) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("watcher did not stop")
		}
	})
	waitMount(t, project, overlayfs.FilesystemType)
	worktree := filepath.Join(root, ".workspaces", "live")
	addWorktree(t, project, "live", worktree)
	waitMount(t, worktree, overlayfs.FilesystemType)
	data, err := os.ReadFile(filepath.Join(worktree, "shared.txt"))
	if err != nil || string(data) != "shared" {
		t.Fatalf("shared overlay: %q, %v", data, err)
	}
	if err := unmountOverlay(context.Background(), worktree, nil); err != nil {
		t.Fatal(err)
	}
	waitMount(t, worktree, "")
	// An explicit unmount stays suppressed while the registration still exists.
	// Two rounds: a mount wrongly started by the first is up by the second.
	awaitReconcile(t, project, root, "first-round")
	awaitReconcile(t, project, root, "second-round")
	waitMount(t, worktree, "")
	metadata, err := exec.Command("git", "-C", worktree, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	// Unregister without deleting through a writable mounted tree.
	name := string(metadata[:len(metadata)-1])
	parked := filepath.Join(project, ".git", "parked-metadata")
	if err := os.Rename(name, parked); err != nil {
		t.Fatal(err)
	}
	// The session retries the worktree only after it has seen it unregistered.
	awaitReconcile(t, project, root, "unregistered")
	if err := os.Rename(parked, name); err != nil {
		t.Fatal(err)
	}
	waitMount(t, worktree, overlayfs.FilesystemType)
	if err := os.Rename(name, parked); err != nil {
		t.Fatal(err)
	}
	waitMount(t, worktree, "")
	if err := os.Rename(parked, name); err != nil {
		t.Fatal(err)
	}
	if err := manageProjects(context.Background(), os.Stdout, "unmount", selection); err != nil {
		t.Fatal(err)
	}
	waitMount(t, project, "")
	// Allow the owner to finish removing its managed exclusion blocks.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(exclude)
		if err == nil && string(data) == string(original) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("exclude file was not restored")
}

// A worktree registered inside the mounted project cannot be mounted. The
// session skips it and keeps serving the project and its other worktrees.
func TestNestedWorktreeIsSkippedWhileMounted(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	shared := scratch.Mkdir(t, filepath.Join(root, ".ai", "shared"))
	selection := Selection{Root: root, Project: "project", Target: project, Worktrees: true,
		Sources: []config.Source{{Name: "shared", Path: shared, Glob: "**/*"}}}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- mountProjects(ctx, selection) }()
	waitMount(t, project, overlayfs.FilesystemType)

	nested := filepath.Join(project, "nested")
	addWorktree(t, project, "nested", nested)
	// Both rounds list the nested worktree. Two rounds: a mount wrongly
	// started by the first is up by the second.
	awaitReconcile(t, project, root, "sibling")
	awaitReconcile(t, project, root, "second-sibling")

	select {
	case err := <-finished:
		t.Fatalf("session stopped over a nested worktree: %v", err)
	default:
	}
	for target, want := range map[string]string{project: overlayfs.FilesystemType, nested: ""} {
		if kind, err := mountedType(context.Background(), target); err != nil || kind != want {
			t.Fatalf("mount at %s: %q, %v; want %q", target, kind, err, want)
		}
	}

	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Errorf("mountProjects returned error on cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mountProjects did not stop")
	}
	waitMount(t, project, "")
}
