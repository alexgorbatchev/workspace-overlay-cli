package workspaceoverlay_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMain(m *testing.M) { scratch.Main(m) }

func recipeFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { cleanupRecipeMounts(t, dir) })
	for _, name := range []string{"justfile", "scripts/test-mode.sh"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scratch.Write(t, filepath.Join(dir, name), string(data))
	}
	scratch.Write(t, filepath.Join(dir, ".tmp/another-session/keep"), "sentinel")
	scratch.Write(t, filepath.Join(dir, "go.mod"), "module recipefixture\n\ngo 1.26.2\n")
	scratch.Write(t, filepath.Join(dir, "value.go"), "package fixture\nfunc value() int { return 1 }\n")
	scratch.Write(t, filepath.Join(dir, "value_test.go"), `package fixture
import ("os"; "testing"; "time")
func TestValue(t *testing.T) {
  _ = time.Second
  mode := os.Getenv("AGENT")
  if mode != "0" && mode != "1" { t.Fatalf("unexpected mode %q", mode) }
  if err := os.WriteFile("mode-"+mode, []byte(mode), 0600); err != nil { t.Fatal(err) }
  `+body+`
}
`)
	return dir
}

func assertScratchClean(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ".tmp"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "another-session" {
		t.Fatalf("scratch changed outside the run: %v, %v", entries, err)
	}
	if got := string(scratch.Read(t, filepath.Join(dir, ".tmp/another-session/keep"))); got != "sentinel" {
		t.Fatalf("other session changed: %q", got)
	}
}

func TestTestRecipeCleansCoverage(t *testing.T) {
	for _, mode := range []string{"success", "test failure", "compile failure", "low coverage"} {
		t.Run(mode, func(t *testing.T) {
			body := "if value() != 1 { t.Fatal(\"wrong value\") }"
			switch mode {
			case "test failure":
				body += "; t.Fatal(\"intentional failure\")"
			case "compile failure":
				body = "undefinedFunction()"
			case "low coverage":
				body = ""
			}
			dir := recipeFixture(t, body)
			cmd := exec.Command("just", "test")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if (err != nil) != (mode != "success") {
				t.Fatalf("recipe failed unexpectedly: %v\n%s", err, out)
			}
			if mode == "low coverage" && !strings.Contains(string(out), "Coverage: 0.0%") {
				t.Fatalf("coverage gate did not evaluate the profile:\n%s", out)
			}
			if mode == "success" {
				for _, agent := range []string{"0", "1"} {
					if got := string(scratch.Read(t, filepath.Join(dir, "mode-"+agent))); got != agent {
						t.Fatalf("missing test mode %s", agent)
					}
				}
			}
			assertScratchClean(t, dir)
		})
	}
}

type recipeRun struct {
	cmd  *exec.Cmd
	out  bytes.Buffer
	done chan struct{}
	err  error
}

func startRecipe(t *testing.T, dir, agent string) *recipeRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	run := &recipeRun{cmd: exec.CommandContext(ctx, "bash", "scripts/test-mode.sh", agent), done: make(chan struct{})}
	run.cmd.Dir = dir
	run.cmd.Stdout, run.cmd.Stderr = &run.out, &run.out
	run.cmd.Cancel = func() error { return run.cmd.Process.Signal(syscall.SIGTERM) }
	run.cmd.WaitDelay = 10 * time.Second
	if err := run.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { run.err = run.cmd.Wait(); close(run.done) }()
	t.Cleanup(func() { cancel(); <-run.done })
	return run
}

func awaitRecipe(t *testing.T, run *recipeRun, marker string) {
	t.Helper()
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		select {
		case <-run.done:
			t.Fatalf("recipe stopped before readiness: %v\n%s", run.err, &run.out)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

const waitingTest = `if value() != 1 { t.Fatal("wrong value") }
for {
  if _, err := os.Stat("release-"+mode); err == nil { break }
  time.Sleep(10*time.Millisecond)
}`

func TestTestModeCleansOnInterruption(t *testing.T) {
	dir := recipeFixture(t, waitingTest)
	run := startRecipe(t, dir, "0")
	awaitRecipe(t, run, filepath.Join(dir, "mode-0"))
	if err := run.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	<-run.done
	if run.err == nil {
		t.Fatal("interrupted test mode succeeded")
	}
	assertScratchClean(t, dir)
}

func TestConcurrentTestModesOwnTheirCoverage(t *testing.T) {
	dir := recipeFixture(t, waitingTest)
	first, second := startRecipe(t, dir, "0"), startRecipe(t, dir, "1")
	awaitRecipe(t, first, filepath.Join(dir, "mode-0"))
	awaitRecipe(t, second, filepath.Join(dir, "mode-1"))
	for i, run := range []*recipeRun{first, second} {
		entries, err := os.ReadDir(filepath.Join(dir, ".tmp"))
		if err != nil || len(entries) != 3-i {
			t.Fatalf("concurrent runs did not keep independent scratch: %v, %v", entries, err)
		}
		scratch.Write(t, filepath.Join(dir, "release-"+[]string{"0", "1"}[i]), "release")
		<-run.done
		if run.err != nil {
			t.Fatalf("concurrent test mode failed: %v\n%s", run.err, &run.out)
		}
	}
	assertScratchClean(t, dir)
}
