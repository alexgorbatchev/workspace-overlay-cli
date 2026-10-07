package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// Keep a pre-mount handle: opening .git through the mounted filesystem would recurse.
type gitExclude struct {
	mu       sync.Mutex
	root     *os.Root
	file     string
	onUpdate func([]byte) error
	marker   string
	block    []byte
}

func openExclude(ctx context.Context, project string) (*gitExclude, error) {
	if _, err := os.Lstat(filepath.Join(project, ".git")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	cmd := projectGit(ctx, project, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("locate Git exclude file: %w", err)
	}
	name := strings.TrimSpace(string(data))
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	return &gitExclude{root: root, file: name, marker: fmt.Sprintf("workspace-overlay %x", rand.Text())}, nil
}

func (v *view) refreshExclude() error {
	if v.exclude == nil {
		return nil
	}
	paths := make(map[string]bool)
	for _, layer := range v.layers[1:] {
		err := iofs.WalkDir(layer.root.FS(), ".", func(name string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if name == "." {
				return nil
			}
			if !layer.includes(name) {
				if entry.IsDir() {
					return iofs.SkipDir
				}
				return nil
			}
			if name == ".git" {
				return iofs.SkipDir
			}
			_, err = v.layers[0].root.Lstat(name)
			if errors.Is(err, os.ErrNotExist) {
				pattern, err := excludePattern(name)
				if err != nil {
					return err
				}
				paths[pattern] = true
				if entry.IsDir() {
					return iofs.SkipDir
				}
				return nil
			}
			return err
		})
		if err != nil {
			return fmt.Errorf("scan overlay exclusions: %w", err)
		}
	}
	patterns := make([]string, 0, len(paths))
	for pattern := range paths {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	return v.exclude.update(patterns)
}

func excludePattern(name string) (string, error) {
	if strings.ContainsAny(name, "\r\n") {
		return "", fmt.Errorf("Git exclude cannot represent path %q", name)
	}
	var pattern strings.Builder
	pattern.WriteByte('/')
	for _, r := range name {
		if strings.ContainsRune("\\*?[]!# ", r) {
			pattern.WriteByte('\\')
		}
		pattern.WriteRune(r)
	}
	return pattern.String(), nil
}

func (g *gitExclude) update(patterns []string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var block []byte
	if len(patterns) > 0 {
		block = []byte("\n# begin " + g.marker + "\n" + strings.Join(patterns, "\n") + "\n# end " + g.marker + "\n")
	}
	if bytes.Equal(block, g.block) {
		return nil
	}
	f, err := g.root.OpenFile("exclude", os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer closeFile(f)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	// Closing the descriptor releases the advisory lock.
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if len(g.block) > 0 {
		if bytes.Count(data, g.block) != 1 {
			return fmt.Errorf("managed Git exclude block changed; refusing to overwrite it")
		}
		data = bytes.Replace(data, g.block, nil, 1)
	}
	data = append(data, block...)
	if _, err := f.WriteAt(data, 0); err != nil {
		return err
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return err
	}
	g.block = block
	if err := f.Sync(); err != nil {
		return err
	}
	if g.onUpdate != nil {
		return g.onUpdate(block)
	}
	return nil
}

func (g *gitExclude) clone() (*gitExclude, error) {
	if g == nil {
		return nil, nil
	}
	root, err := g.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	return &gitExclude{root: root, file: g.file, marker: fmt.Sprintf("workspace-overlay %x", rand.Text())}, nil
}

func (g *gitExclude) close() error {
	if g == nil {
		return nil
	}
	err := g.update(nil)
	return errors.Join(err, g.root.Close())
}
