package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"workspace-overlay/internal/config"
	"workspace-overlay/internal/session"

	helptree "github.com/alexgorbatchev/cobra-help-tree/v2"
	"github.com/spf13/cobra"
)

//go:embed SKILL.md
var skill string

var version = "0.1.0"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	root, err := newRootCommand()
	if err != nil {
		return err
	}
	// Usage helps with a mistyped command line, not with a failed operation, so
	// it is shown only when the arguments never reached a command. Cobra would
	// print it to the output writer; it is a diagnostic and belongs on stderr.
	parsed := false
	root.PersistentPreRun = func(*cobra.Command, []string) { parsed = true }
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, err := root.ExecuteContextC(ctx)
	if err != nil && !parsed {
		if _, writeErr := fmt.Fprintln(stderr, cmd.UsageString()); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	return err
}

// newRootCommand builds the complete command tree; tests inspect what it returns.
func newRootCommand() (*cobra.Command, error) {
	root := &cobra.Command{Use: "workspace-overlay", Short: "Serve agent files from a workspace source", Version: version, SilenceErrors: true, SilenceUsage: true}
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
			cfg, err := config.Load(configFile)
			if err != nil {
				return err
			}
			selections, err := session.Selections(cfg, project, replace, worktrees)
			if err != nil {
				return err
			}
			if action == "mount" {
				return session.Mount(cmd.Context(), selections)
			}
			var result error
			for _, selection := range selections {
				if action == "status" {
					result = errors.Join(result, session.Status(cmd.Context(), cmd.OutOrStdout(), selection))
				} else {
					result = errors.Join(result, session.Unmount(cmd.Context(), selection))
				}
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
		return nil, err
	}
	help := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if helptree.IsAgentMode() {
			fmt.Fprintln(cmd.OutOrStdout(), "ALERT: Agents must read `AGENT=1 workspace-overlay skill` before using this tool.")
		}
		help(cmd, args)
	})
	return root, nil
}
