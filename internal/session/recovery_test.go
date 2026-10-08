package session

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/registry"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestOwnerProcessHelper(t *testing.T) {
	configEnv := os.Getenv("WORKSPACE_OVERLAY_OWNER_CONFIG")
	if configEnv == "" {
		return
	}
	cfg, err := config.Load(configEnv)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := Selections(cfg, "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	// Serves until the parent test kills this process.
	if err := Mount(context.Background(), owned); err != nil {
		t.Fatal(err)
	}
}

// processOutput collects what a child process prints while the test reads it.
type processOutput struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (o *processOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.Write(p)
}

func (o *processOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

// startOwner serves a configuration from another process, the way a real
// session runs, so a test can kill it and leave its mounts behind. It returns
// once the owner has reported that many targets as mounted.
func startOwner(t *testing.T, configFile string, mounts int) (*exec.Cmd, *processOutput) {
	t.Helper()
	owner := exec.Command(os.Args[0], "-test.run=^TestOwnerProcessHelper$")
	owner.Env = append(os.Environ(), "WORKSPACE_OVERLAY_OWNER_CONFIG="+configFile)
	output := &processOutput{}
	owner.Stdout = output
	owner.Stderr = output
	if err := owner.Start(); err != nil {
		t.Fatalf("start owner process: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(output.String(), "Mounted ") < mounts {
		if time.Now().After(deadline) {
			t.Fatalf("owner did not report %d mounts:\n%s", mounts, output)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return owner, output
}

func TestReplaceRecoversDeadOwner(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	scratch.GitRepo(t, project)
	worktree := filepath.Join(root, ".workspaces", "one", "project")
	addWorktree(t, project, "linked", worktree)
	scratch.Write(t, filepath.Join(root, "ai", "overlay-only.md"), "overlay\n")
	configFile := scratch.Write(t, filepath.Join(root, config.Name), `version=1
[projects.project]
path='project'
[[overlays]]
name='ai'
source='ai'
projects=['*']
`)
	targets := []string{project, worktree}

	owner, ownerOutput := startOwner(t, configFile, len(targets))
	for _, target := range targets {
		waitMount(t, target, overlayfs.FilesystemType)
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatalf("kill owner: %v", err)
	}
	_ = owner.Wait() // a killed process always reports an error
	t.Cleanup(func() {
		for _, target := range targets {
			if err := unmountOverlay(context.Background(), target); err != nil {
				t.Logf("cleanup unmount: %v", err)
			}
		}
	})

	// The owner is gone but its mounts remain: still listed, no longer served.
	if kind, err := mountedType(context.Background(), project); err != nil || kind != overlayfs.FilesystemType {
		t.Fatalf("dead mount: %q, %v; want it still listed as %s", kind, err, overlayfs.FilesystemType)
	}
	if _, err := os.Lstat(filepath.Join(project, ".git")); err == nil {
		t.Fatal("dead mount still answers")
	}

	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	replacing, err := Selections(cfg, "", true, true)
	if err != nil {
		t.Fatalf("get selections: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- Mount(ctx, replacing) }()

	served := func() bool {
		for _, target := range targets {
			// Fails while the dead mount or no mount is there, and prepares the
			// new session's mount for the read that follows.
			if selfReadable(target) != nil {
				return false
			}
			if data, err := os.ReadFile(filepath.Join(target, "overlay-only.md")); err != nil || string(data) != "overlay\n" {
				return false
			}
		}
		return true
	}
	deadline := time.After(10 * time.Second)
	for !served() {
		select {
		case err := <-finished:
			t.Fatalf("Mount returned before serving the overlay: %v\nowner output:\n%s", err, ownerOutput)
		case <-deadline:
			t.Fatalf("overlay not served 10 s after mount --replace\nowner output:\n%s", ownerOutput)
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Errorf("Mount after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Mount did not stop after cancel")
	}
	for _, target := range targets {
		waitMount(t, target, "")
	}
	state, err := registry.Read(root, "project")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if state.Version != 0 || len(state.Mounts) != 0 {
		t.Fatalf("registry not cleaned: %+v", state)
	}
}

// abortMount ends the FUSE connection behind target, which releases a process
// that is stuck waiting on it.
func abortMount(t *testing.T, target string) {
	t.Helper()
	table, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Error(err)
		return
	}
	for line := range strings.Lines(string(table)) {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] != target {
			continue
		}
		_, connection, _ := strings.Cut(fields[2], ":")
		if err := os.WriteFile(filepath.Join("/sys/fs/fuse/connections", connection, "abort"), []byte("1"), 0); err != nil {
			t.Errorf("abort the connection of %s: %v", target, err)
		}
	}
}

// An owner that is killed must exit, whenever the kill arrives. A process
// killed in the middle of a file operation on a mount it serves never does:
// the request waits for an answer only that process could give. The first
// milliseconds after a mount appears are where such an operation used to be,
// so that is where the kill is aimed.
func TestKilledOwnerAlwaysExits(t *testing.T) {
	delays := []time.Duration{300 * time.Microsecond, 600 * time.Microsecond, time.Millisecond}
	for round := range 4 * len(delays) {
		delay := delays[round%len(delays)]
		root := t.TempDir()
		project := filepath.Join(root, "project")
		scratch.GitRepo(t, project)
		worktree := filepath.Join(root, ".workspaces", "one", "project")
		addWorktree(t, project, "linked", worktree)
		scratch.Write(t, filepath.Join(root, "ai", "overlay-only.md"), "overlay\n")
		configFile := scratch.Write(t, filepath.Join(root, config.Name), "version=1\n[projects.project]\npath='project'\n[[overlays]]\nname='ai'\nsource='ai'\nprojects=['*']\n")
		targets := []string{project, worktree}

		owner, output := startOwner(t, configFile, 0)
		listed := func() bool {
			table, err := os.ReadFile("/proc/self/mountinfo")
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range targets {
				if !strings.Contains(string(table), " "+target+" ") {
					return false
				}
			}
			return true
		}
		for deadline := time.Now().Add(10 * time.Second); !listed(); {
			if time.Now().After(deadline) {
				t.Fatalf("mounts never appeared:\n%s", output)
			}
		}
		// Sleeping is too coarse for a window this narrow.
		for start := time.Now(); time.Since(start) < delay; {
		}
		if err := owner.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() {
			_ = owner.Wait() // a killed process always reports an error
			close(exited)
		}()
		stuck := false
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			stuck = true
			for _, target := range targets {
				abortMount(t, target)
			}
			<-exited
		}
		for _, target := range targets {
			if err := unmountOverlay(context.Background(), target); err != nil {
				t.Errorf("remove the dead mount %s: %v", target, err)
			}
		}
		if stuck {
			t.Fatalf("round %d: owner killed %s after its mounts appeared did not exit", round, delay)
		}
	}
}

func TestOwnerClearsLeftoverStopRequest(t *testing.T) {
	root := t.TempDir()
	dir, _, stop, err := registry.Paths(root, "project")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(stop, nil, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stop); err != nil {
		t.Fatalf("stop file not created: %v", err)
	}

	// registry.Open should succeed and clear the stop file
	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatalf("registry.Open failed: %v", err)
	}

	if _, err := os.Stat(stop); !os.IsNotExist(err) {
		if err != nil {
			t.Fatalf("stat stop file: %v", err)
		}
		t.Fatal("stop file was not removed by registry.Open")
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close registry: %v", err)
	}
}

func TestStopRequestRemovedOnceOwnerIsGone(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "project")

	r, err := registry.Open(root, "project")
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}

	if err := r.Record(target, "", nil); err != nil {
		t.Fatalf("Record: %v", err)
	}

	finished := make(chan error, 1)
	go func() {
		finished <- stopRegistered(context.Background(), root, "project")
	}()

	// Poll until stop file is created
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()

	stopExists := false
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(r.StopFile()); err == nil {
				stopExists = true
			}
		case <-deadline.C:
			t.Fatal("stop file was not created")
		}
		if stopExists {
			break
		}
	}

	// Remove the registry file to model an owner that cleaned up but left the stop file
	if err := os.Remove(r.File()); err != nil {
		t.Fatalf("remove registry file: %v", err)
	}

	var stopErr error
	select {
	case stopErr = <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("stopRegistered did not complete within 2 seconds")
	}

	if stopErr != nil {
		t.Fatalf("stopRegistered returned error: %v", stopErr)
	}

	if _, err := os.Stat(r.StopFile()); !os.IsNotExist(err) {
		if err != nil {
			t.Fatalf("stat stop file: %v", err)
		}
		t.Fatal("stop file was not removed after owner gone")
	}

	if err := r.Forget(target); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
