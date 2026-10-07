package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/fixture"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

func fixtureCommand() *cobra.Command {
	group := &cobra.Command{Use: "fixture", Short: "Manage an isolated verification workspace"}
	var directory string
	create := &cobra.Command{Use: "create", Short: "Create or reuse mock projects and Git worktrees", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		configFile, err := fixture.Create(cmd.Context(), directory)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), pathname.Display(configFile))
		return err
	}}
	create.Flags().StringVar(&directory, "directory", ".tmp/dev-workspace", "Workspace directory (relative to current directory)")
	group.AddCommand(create)
	return group
}
