package workspaceoverlay_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/overlayfs"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

// Keep failed regression runs from stranding a dead mount in t.TempDir.
func cleanupRecipeMounts(t *testing.T, dir string) {
	t.Helper()
	out, err := exec.Command("findmnt", "--json", "--list", "--output", "TARGET,FSTYPE").Output()
	if err != nil {
		t.Error(err)
		return
	}
	var table struct {
		Filesystems []struct {
			Target string `json:"target"`
			Type   string `json:"fstype"`
		} `json:"filesystems"`
	}
	if err := json.Unmarshal(out, &table); err != nil {
		t.Error(err)
		return
	}
	for _, mount := range table.Filesystems {
		if strings.HasPrefix(mount.Target, dir+string(filepath.Separator)) && mount.Type == overlayfs.FilesystemType {
			if out, err := exec.Command("fusermount3", "-u", "-z", "--", mount.Target).CombinedOutput(); err != nil {
				t.Errorf("clean failed fixture mount: %v\n%s", err, out)
			}
		}
	}
}

func TestTestModeCleansInterruptedMount(t *testing.T) {
	body := fmt.Sprintf(`if value() != 1 { t.Fatal("wrong value") }
cmd := exec.Command(%q, "-test.run=^TestRecipeMountProcess$")
cmd.Env = append(os.Environ(), "WORKSPACE_OVERLAY_RECIPE_MOUNT="+mustAbs(t, "mounted"))
if out, err := cmd.CombinedOutput(); err != nil { t.Fatalf("mount helper: %%v\n%%s", err, out) }`, os.Args[0])
	dir := recipeFixture(t, body)
	path := filepath.Join(dir, "value_test.go")
	source := strings.Replace(string(scratch.Read(t, path)), `"os";`, `"os"; "os/exec"; "path/filepath";`, 1)
	source += `
func mustAbs(t *testing.T, name string) string {
  t.Helper()
  path, err := filepath.Abs(name)
  if err != nil { t.Fatal(err) }
  return path
}
`
	scratch.Write(t, path, source)
	run := startRecipe(t, dir, "0")
	marker := filepath.Join(dir, "mounted")
	awaitRecipe(t, run, marker)
	pid, err := strconv.Atoi(strings.TrimSpace(string(scratch.Read(t, marker))))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	<-run.done
	if run.err == nil {
		t.Fatal("interrupted mount test succeeded")
	}
	assertScratchClean(t, dir)
	// The runner must stop the native server, not merely detach its mount.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mount helper %d survived cleanup: %v", pid, err)
		}
	}
}

func TestRecipeMountProcess(t *testing.T) {
	marker := os.Getenv("WORKSPACE_OVERLAY_RECIPE_MOUNT")
	if marker == "" {
		return
	}
	project := filepath.Join(t.TempDir(), "project with spaces")
	scratch.Mkdir(t, project)
	root, err := os.OpenRoot(project)
	if err != nil {
		t.Fatal(err)
	}
	view := &overlayfs.View{}
	view.AddProject(root)
	t.Cleanup(view.Close)
	server, err := view.Mount(project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
	})
	scratch.Write(t, filepath.Join(os.Getenv("XDG_STATE_HOME"), "leftover"), "state")
	scratch.Write(t, marker, strconv.Itoa(os.Getpid()))
	for {
		time.Sleep(time.Second)
	}
}
