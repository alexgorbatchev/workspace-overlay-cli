package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestFixtureRun(t *testing.T) {
	t.Run("creates workspace with default argument", func(t *testing.T) {
		temp := t.TempDir()
		t.Chdir(temp)
		var stdout, stderr bytes.Buffer
		if err := run(nil, &stdout, &stderr); err != nil {
			t.Fatalf("run failed: %v", err)
		}
		if !strings.Contains(stdout.String(), "dev-workspace/workspace-overlay.toml") {
			t.Errorf("expected config path in stdout, got %q", stdout.String())
		}
	})

	t.Run("creates workspace with explicit directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "custom-workspace")
		var stdout, stderr bytes.Buffer
		if err := run([]string{dir}, &stdout, &stderr); err != nil {
			t.Fatalf("run failed: %v", err)
		}
		if !strings.Contains(stdout.String(), "custom-workspace/workspace-overlay.toml") {
			t.Errorf("expected config path in stdout, got %q", stdout.String())
		}
	})

	t.Run("fails when directory cannot be created", func(t *testing.T) {
		fileParent := filepath.Join(t.TempDir(), "not-a-dir")
		scratch.Write(t, fileParent, "blocking file")
		invalidTarget := filepath.Join(fileParent, "workspace")
		var stdout, stderr bytes.Buffer
		if err := run([]string{invalidTarget}, &stdout, &stderr); err == nil {
			t.Error("expected error when target parent is a file")
		}
		if !strings.Contains(stderr.String(), "ERR:") {
			t.Errorf("expected error message in stderr, got %q", stderr.String())
		}
	})
}
