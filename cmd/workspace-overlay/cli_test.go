package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRuntimeErrorOmitsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.toml")
	if err := run([]string{"overlay", "status", "--config", missing}, &stdout, &stderr); err == nil {
		t.Fatal("missing configuration accepted")
	}
	if output := stdout.String() + stderr.String(); strings.Contains(output, "Usage:") {
		t.Fatalf("failed operation printed usage: %q", output)
	}
}

func TestCommandLineMistakeShowsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"overlay", "status", "--no-such-flag"}, &stdout, &stderr); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("mistyped command line printed no usage: %q", stderr.String())
	}
	// Usage after an error is a diagnostic: stdout stays clean for pipelines.
	if stdout.Len() != 0 {
		t.Fatalf("diagnostics leaked to stdout: %q", stdout.String())
	}
}

func TestUsageWriteFailureIsReported(t *testing.T) {
	var stdout bytes.Buffer
	err := run([]string{"overlay", "status", "--no-such-flag"}, &stdout, &failingWriter{})
	if err == nil || !strings.Contains(err.Error(), "no-such-flag") || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("flag error and failed usage write: %v", err)
	}
}

// each collects what a visitor passes to its callback, so flag sets can be
// ranged over here without a direct dependency on the flag library.
func each[T any](visit func(func(T))) []T {
	var items []T
	visit(func(item T) { items = append(items, item) })
	return items
}

// withHelpFlags adds the help flag Cobra otherwise adds lazily at execution
// time, so the walk sees the flags a user sees.
func withHelpFlags(cmd *cobra.Command) {
	cmd.InitDefaultHelpFlag()
	for _, sub := range cmd.Commands() {
		withHelpFlags(sub)
	}
}

// undocumented lists commands and flags of the live command tree that the
// embedded guide does not mention. The guide writes names in backticks.
func undocumented(root *cobra.Command, guide string) []string {
	root.InitDefaultHelpCmd()
	root.InitDefaultVersionFlag()
	withHelpFlags(root)
	var missing []string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		name := strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), root.Name()))
		scope := name
		if cmd == root {
			scope = root.Name()
		} else if !strings.Contains(guide, "`"+name+"`") && !strings.Contains(guide, "`"+name+" ") {
			missing = append(missing, "command "+name)
		}
		for _, flag := range each(cmd.LocalFlags().VisitAll) {
			long := strings.Contains(guide, "--"+flag.Name+"`")
			short := flag.Shorthand == "" || strings.Contains(guide, "`-"+flag.Shorthand+"`")
			if !long || !short {
				missing = append(missing, "flag --"+flag.Name+" of "+scope)
			}
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return missing
}

func TestGuideDocumentsEveryCommandAndFlag(t *testing.T) {
	root, err := newRootCommand()
	if err != nil {
		t.Fatal(err)
	}
	if missing := undocumented(root, skill); len(missing) != 0 {
		t.Fatalf("embedded guide does not mention: %s", strings.Join(missing, "; "))
	}
}

func TestGuideDriftIsDetected(t *testing.T) {
	cases := []struct{ name, documented, renamed, want string }{
		{"flag", "--replace", "--removed", "flag --replace of overlay mount"},
		{"command", "`overlay status`", "`overlay state`", "command overlay status"},
		{"shorthand", "`-v`", "`-V`", "flag --version of workspace-overlay"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root, err := newRootCommand()
			if err != nil {
				t.Fatal(err)
			}
			missing := undocumented(root, strings.ReplaceAll(skill, tt.documented, tt.renamed))
			if !slices.Contains(missing, tt.want) {
				t.Fatalf("drift %q not reported: %v", tt.want, missing)
			}
		})
	}
}
