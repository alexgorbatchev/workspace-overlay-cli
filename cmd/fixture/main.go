package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/fixture"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

func run(args []string, stdout, stderr io.Writer) error {
	dir := "dev-workspace"
	if len(args) > 0 {
		dir = args[0]
	}
	config, err := fixture.Create(context.Background(), dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ERR: %v\n", err)
		return err
	}
	_, err = fmt.Fprintln(stdout, pathname.Display(config))
	return err
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}
