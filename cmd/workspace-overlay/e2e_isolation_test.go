package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/fixture"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitrepo"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

// Re-execute the actual E2E test from a disposable checkout layout. A regression
// must never run the old destructive cleanup against someone's real fixture.
func TestE2ELeavesInteractiveFixtureUntouched(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "early-failure"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "dev-workspace")
			if _, err := fixture.Create(t.Context(), workspace); err != nil {
				t.Fatal(err)
			}
			for _, project := range []string{"alpha", "beta"} {
				dir := filepath.Join(workspace, project)
				if out, err := gitrepo.Command(t.Context(), dir, "checkout", "-b", "user-work").CombinedOutput(); err != nil {
					t.Fatalf("prepare branch: %v: %s", err, out)
				}
				scratch.Write(t, filepath.Join(dir, "AGENTS.md"), "uncommitted user edits\n")
				scratch.Write(t, filepath.Join(dir, "untracked.txt"), "untracked user file\n")
			}
			scratch.Write(t, filepath.Join(workspace, ".ai", "workspace", "AGENTS.md"), "user overlay edits\n")
			before := interactiveFixtureState(t, workspace)
			cwd := scratch.Mkdir(t, filepath.Join(root, "cmd", "workspace-overlay"))
			runDir := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestE2ELocalWorkspace$", "-test.timeout=45s")
			cmd.Dir = cwd
			cmd.Env = append(os.Environ(), "TMPDIR="+runDir)
			cmd.WaitDelay = e2eGrace
			if fail {
				// Fail the first Git status assertion after real mounts exist.
				bin := scratch.Mkdir(t, filepath.Join(root, "bin"))
				wrapper := scratch.Write(t, filepath.Join(bin, "git"), "#!/bin/sh\ncase \" $* \" in *' status --porcelain '*) echo injected-status-failure >&2; exit 1;; esac\nexec \"$E2E_REAL_GIT\" \"$@\"\n")
				if err := os.Chmod(wrapper, 0755); err != nil {
					t.Fatal(err)
				}
				cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "E2E_REAL_GIT="+git)
			}
			out, err := cmd.CombinedOutput()
			if fail {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(out), "injected-status-failure") {
					t.Errorf("injected failure = %v: %s", err, out)
				}
			} else if err != nil {
				t.Errorf("E2E with a dirty interactive fixture: %v: %s", err, out)
			}
			if after := interactiveFixtureState(t, workspace); !reflect.DeepEqual(before, after) {
				t.Errorf("E2E changed the interactive fixture\nbefore: %q\nafter: %q", before, after)
			}
			if entries, err := os.ReadDir(runDir); err != nil || len(entries) != 0 {
				t.Errorf("E2E left scratch behind: %v, %v", entries, err)
			}
		})
	}
}

func interactiveFixtureState(t *testing.T, workspace string) map[string]string {
	t.Helper()
	state := map[string]string{}
	for _, project := range []string{"alpha", "beta"} {
		dir := filepath.Join(workspace, project)
		for _, args := range [][]string{{"status", "--porcelain"}, {"symbolic-ref", "HEAD"}, {"show-ref"}} {
			out, err := gitrepo.Command(t.Context(), dir, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("read fixture state: %v: %s", err, out)
			}
			state[project+"/"+strings.Join(args, " ")] = string(out)
		}
		for _, file := range []string{"AGENTS.md", "untracked.txt"} {
			data, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			state[project+"/"+file] = string(data)
		}
	}
	state["overlay"] = string(scratch.Read(t, filepath.Join(workspace, ".ai", "workspace", "AGENTS.md")))
	return state
}
