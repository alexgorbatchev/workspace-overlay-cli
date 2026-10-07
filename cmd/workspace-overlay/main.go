package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	helptree "github.com/alexgorbatchev/cobra-help-tree/v2"
	"github.com/spf13/cobra"
)

//go:embed SKILL.md
var skill string

var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(1)
	}
}

func run() error {
	root := &cobra.Command{Use: "workspace-overlay", Short: "Serve agent files from a workspace source", Version: version, SilenceErrors: true}
	root.SetVersionTemplate("{{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true
	root.RunE = func(cmd *cobra.Command, args []string) error { return cmd.Help() }
	root.AddCommand(&cobra.Command{Use: "skill", Short: "Print the embedded operating guide", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		_, err := fmt.Fprint(cmd.OutOrStdout(), skill)
		return err
	}})
	var configFile, project string
	var replace bool
	overlay := &cobra.Command{Use: "overlay", Short: "Manage configured project overlays"}
	overlay.PersistentFlags().StringVar(&configFile, "config", "", "TOML file (default: discover workspace-overlay.toml in current or parent directories)")
	overlay.PersistentFlags().StringVar(&project, "project", "", "Configured project name (default: all projects)")
	for _, action := range []string{"mount", "unmount", "status"} {
		var worktrees bool
		command := &cobra.Command{Use: action, Short: action + " configured project overlays", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			config, err := loadConfig(configFile)
			if err != nil {
				return err
			}
			selections, err := config.selections(project, replace, worktrees)
			if err != nil {
				return err
			}
			if action == "mount" {
				return mountSelections(cmd.Context(), selections)
			}
			var result error
			for _, selection := range selections {
				result = errors.Join(result, manageProjects(cmd.Context(), cmd.OutOrStdout(), action, selection))
			}
			return result
		}}
		command.Flags().BoolVar(&worktrees, "worktrees", true, "Include and monitor Git worktrees of each project")
		if action == "mount" {
			command.Flags().BoolVar(&replace, "replace", false, "Stop existing overlays before starting")
		}
		overlay.AddCommand(command)
	}
	root.AddCommand(overlay)
	root.AddCommand(fixtureCommand())
	opts := helptree.HelpOptions{Catalog: helptree.TechCatalog{}, Tree: helptree.TreeOptions{HideGeneratedCommands: true}}
	if err := helptree.SetupWithOptions(root, opts); err != nil {
		return err
	}
	help := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENT"))) {
		case "1", "true", "yes":
			fmt.Fprintln(cmd.OutOrStdout(), "ALERT: Agents must read `AGENT=1 workspace-overlay skill` before using this tool.")
		}
		help(cmd, args)
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return root.ExecuteContext(ctx)
}

func projectPath(root, project string) (string, error) {
	if project == "." || project == ".." || strings.ContainsAny(project, "/\\") || project == "" {
		return "", fmt.Errorf("project must be a directory name")
	}
	return filepath.Abs(filepath.Join(root, project))
}
