package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

//go:embed all:testdata/workspace
var fixtureFiles embed.FS

func fixtureCommand() *cobra.Command {
	group := &cobra.Command{Use: "fixture", Short: "Manage an isolated verification workspace"}
	var directory string
	create := &cobra.Command{Use: "create", Short: "Create or reuse mock projects and Git worktrees", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		config, err := createFixture(cmd.Context(), directory)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), displayPath(config))
		return err
	}}
	create.Flags().StringVar(&directory, "directory", ".tmp/dev-workspace", "Workspace directory (relative to current directory)")
	group.AddCommand(create)
	return group
}

func createFixture(ctx context.Context, directory string) (string, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	config := filepath.Join(root, configName)
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
			return "", fmt.Errorf("refusing to overwrite existing or incomplete fixture directory: %s", displayPath(root))
		}
		if _, err := loadConfig(config); err != nil {
			return "", err
		}
		return config, nil
	}
	if err := copyFixture(root); err != nil {
		return "", err
	}
	for _, project := range []string{"alpha", "beta"} {
		target := filepath.Join(root, project)
		for _, args := range [][]string{{"init", "--initial-branch=main"}, {"add", "."}, {"commit", "-m", "Initialize verification fixture"}, {"worktree", "add", "-b", "verification", filepath.Join(root, ".workspaces", "one", project), "main"}} {
			if err := fixtureGit(ctx, target, args...); err != nil {
				return "", err
			}
		}
	}
	if err := os.WriteFile(marker, []byte("workspace-overlay fixture v1\n"), 0644); err != nil {
		return "", err
	}
	return config, nil
}

func copyFixture(root string) error {
	const source = "testdata/workspace"
	return fs.WalkDir(fixtureFiles, source, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(root, strings.TrimPrefix(name, source))
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := fixtureFiles.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

func fixtureGit(ctx context.Context, target string, args ...string) error {
	options := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=Workspace Fixture", "-c", "user.email=fixture@example.invalid"}
	cmd := projectGit(ctx, target, append(options, args...)...)
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
