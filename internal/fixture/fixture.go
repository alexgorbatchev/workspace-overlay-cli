// Package fixture creates the mock workspace used to verify the tool by hand
// and in integration tests.
package fixture

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitrepo"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

//go:embed all:testdata/workspace
var files embed.FS

// Create builds the mock workspace in directory, or reuses a complete one
// without touching its files, and returns the path of its configuration.
func Create(ctx context.Context, directory string) (string, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	configFile := filepath.Join(root, config.Name)
	marker := filepath.Join(root, ".workspace-overlay-fixture")
	if err := os.MkdirAll(filepath.Dir(root), 0755); err != nil {
		return "", err
	}
	if err := os.Mkdir(root, 0755); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != "workspace-overlay fixture v1\n" {
			return "", fmt.Errorf("refusing to overwrite existing or incomplete fixture directory: %s", pathname.Display(root))
		}
		if _, err := config.Load(configFile); err != nil {
			return "", err
		}
		return configFile, nil
	}
	if err := populate(root); err != nil {
		return "", err
	}
	for _, project := range []string{"alpha", "beta"} {
		target := filepath.Join(root, project)
		for _, args := range [][]string{{"init", "--initial-branch=main"}, {"add", "."}, {"commit", "-m", "Initialize verification fixture"}, {"worktree", "add", "-b", "verification", filepath.Join(root, ".workspaces", "one", project), "main"}} {
			if err := git(ctx, target, args...); err != nil {
				return "", err
			}
		}
	}
	if err := os.WriteFile(marker, []byte("workspace-overlay fixture v1\n"), 0644); err != nil {
		return "", err
	}
	return configFile, nil
}

func populate(root string) error {
	const source = "testdata/workspace"
	return fs.WalkDir(files, source, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(root, strings.TrimPrefix(name, source))
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

func git(ctx context.Context, target string, args ...string) error {
	options := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Workspace Fixture", "-c", "user.email=fixture@example.invalid"}
	cmd := gitrepo.Command(ctx, target, append(options, args...)...)
	// Inherited Git overrides must never redirect fixture writes to another repository.
	cmd.Env = []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fixture git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}
