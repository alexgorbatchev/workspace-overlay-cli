package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func serve(ctx context.Context, workspaceRoot, workspace, project string) (result error) {
	mountpoint, err := projectPath(workspaceRoot, project)
	if err != nil {
		return err
	}
	shared, err := projectPath(filepath.Join(workspaceRoot, ".ai"), workspace)
	if err != nil {
		return err
	}
	specific, err := projectPath(filepath.Join(workspaceRoot, ".ai"), project)
	if err != nil {
		return err
	}
	if shared == specific {
		return fmt.Errorf("workspace and project layer names must differ")
	}
	kind, err := mountedType(ctx, mountpoint)
	if err != nil {
		return err
	}
	if kind != "" {
		return fmt.Errorf("project is already mounted as %s; use --replace for an overlay", kind)
	}
	// Open all directory handles before mounting so the project remains accessible underneath.
	v := &view{}
	defer v.close()
	for _, source := range []string{mountpoint, shared, specific} {
		backing, err := os.OpenRoot(source)
		if err != nil {
			return fmt.Errorf("open layer %s: %w", source, err)
		}
		v.layers = append(v.layers, layer{root: backing})
	}
	v.exclude, err = openExclude(ctx, mountpoint)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, v.exclude.close()) }()
	if err := v.refreshExclude(); err != nil {
		return err
	}
	zero := time.Duration(0)
	opts := &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"default_permissions"}, FsName: "workspace-overlay", Name: "workspace-overlay"}, EntryTimeout: &zero, AttrTimeout: &zero, NegativeTimeout: &zero}
	server, err := fs.Mount(mountpoint, &node{view: v, path: "."}, opts)
	if err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Mounted %s + %s + %s; Ctrl-C to unmount.\n", displayPath(mountpoint), displayPath(shared), displayPath(specific))
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
