package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func serve(ctx context.Context, plan mountPlan) error {
	zero := time.Duration(0)
	opts := &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"default_permissions"}, FsName: "workspace-overlay", Name: "workspace-overlay"}, EntryTimeout: &zero, AttrTimeout: &zero, NegativeTimeout: &zero}
	server, err := fs.Mount(plan.target, &node{view: plan.view, path: "."}, opts)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	paths := []string{displayPath(plan.target)}
	for _, source := range plan.sources {
		paths = append(paths, displayPath(source.path))
	}
	fmt.Fprintf(os.Stderr, "Mounted %s; Ctrl-C to unmount.\n", strings.Join(paths, " + "))
	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		if err := server.Unmount(); err != nil {
			return fmt.Errorf("unmount overlay: %w", err)
		}
		<-done
		return nil
	}
}

func displayPath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return name
	}
	rel, err := filepath.Rel(home, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return name
	}
	if rel == "." {
		return "~"
	}
	return "~" + string(filepath.Separator) + rel
}
