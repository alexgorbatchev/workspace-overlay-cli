package main

import (
	"context"
	_ "embed"
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(1)
	}
}

func run() error {
	root := &cobra.Command{Use: "workspace-overlay", Short: "Serve agent files from a workspace source", Version: "poc", SilenceErrors: true}
	root.SetVersionTemplate("{{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true
	root.RunE = func(cmd *cobra.Command, args []string) error { return cmd.Help() }
	root.AddCommand(&cobra.Command{Use: "skill", Short: "Print the embedded operating guide", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		_, err := fmt.Fprint(cmd.OutOrStdout(), skill)
		return err
	}})
	var workspaceRoot, workspace, project string
	const defaultRoot = ".."
	const defaultWorkspace = "workspace"
	const defaultProject = "alpha"
	var replace bool
	mount := &cobra.Command{Use: "mount", Short: "Serve the merged project until interrupted", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		mountpoint, err := projectPath(workspaceRoot, project)
		if err != nil {
			return err
		}
		if replace {
			if err := unmountOverlay(cmd.Context(), mountpoint); err != nil {
				return err
			}
		}
		return serve(cmd.Context(), workspaceRoot, workspace, project)
	}}
	mount.Flags().BoolVar(&replace, "replace", false, "Unmount an existing overlay before starting")
	mount.Flags().StringVar(&workspaceRoot, "root", defaultRoot, "Workspace directory (relative to current directory)")
	mount.Flags().StringVar(&workspace, "workspace", defaultWorkspace, "Shared layer name under .ai")
	mount.Flags().StringVar(&project, "project", defaultProject, "Project directory and layer name")
	overlay := &cobra.Command{Use: "overlay", Short: "Manage the virtual agent directory"}
	overlay.AddCommand(mount)
	for _, action := range []string{"unmount", "status"} {
		var targetRoot, targetProject string
		command := &cobra.Command{Use: action, Short: action + " the agent directory", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			target, err := projectPath(targetRoot, targetProject)
			if err != nil {
				return err
			}
			if action == "unmount" {
				return unmountOverlay(cmd.Context(), target)
			}
			kind, err := mountedType(cmd.Context(), target)
			if err != nil {
				return err
			}
			if kind == "" {
				kind = "unmounted"
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), kind)
			return err
		}}
		command.Flags().StringVar(&targetRoot, "root", defaultRoot, "Workspace directory (relative to current directory)")
		command.Flags().StringVar(&targetProject, "project", defaultProject, "Project directory to inspect or unmount")
		overlay.AddCommand(command)
	}
	root.AddCommand(overlay)
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
