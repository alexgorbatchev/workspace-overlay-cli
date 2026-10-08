package session

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/fsnotify/fsnotify"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitrepo"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/subprocess"
)

type notificationRoot struct {
	file *os.File
	git  bool
}

type notifications struct {
	watcher *fsnotify.Watcher
	roots   []notificationRoot
	control string
}

func newNotifications(ctx context.Context, plan mountPlan, worktrees bool) (*notifications, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	n := &notifications{watcher: watcher}
	if worktrees {
		if _, err := plan.view.Project().Lstat(".git"); err == nil {
			cmd := gitrepo.Command(ctx, plan.target, "rev-parse", "--path-format=absolute", "--git-common-dir")
			data, err := subprocess.Output(ctx, cmd)
			if err != nil {
				return nil, errors.Join(err, n.close())
			}
			file, err := os.Open(strings.TrimSpace(string(data)))
			if err != nil {
				return nil, errors.Join(err, n.close())
			}
			n.roots = append(n.roots, notificationRoot{file: file, git: true})
		} else if !os.IsNotExist(err) {
			return nil, errors.Join(err, n.close())
		}
	}
	for _, overlayRoot := range plan.view.Overlays() {
		file, err := overlayRoot.Open(".")
		if err != nil {
			return nil, errors.Join(err, n.close())
		}
		n.roots = append(n.roots, notificationRoot{file: file})
	}
	if err := n.sync(); err != nil {
		return nil, errors.Join(err, n.close())
	}
	return n, nil
}

func nativePath(file *os.File) string { return fmt.Sprintf("/proc/self/fd/%d", file.Fd()) }

func (n *notifications) sync() error {
	desired := make(map[string]bool)
	if n.control != "" {
		desired[n.control] = true
	}
	for _, root := range n.roots {
		base := nativePath(root.file)
		if root.git {
			desired[base] = true
			entries, err := os.ReadDir(filepath.Join(base, "worktrees"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			desired[filepath.Join(base, "worktrees")] = true
			for _, entry := range entries {
				if entry.IsDir() {
					desired[filepath.Join(base, "worktrees", entry.Name())] = true
				}
			}
			continue
		}
		if err := filepath.WalkDir(base+"/.", func(name string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			if entry.Name() == ".git" {
				return iofs.SkipDir
			}
			desired[filepath.Clean(name)] = true
			return nil
		}); err != nil {
			return err
		}
	}
	current := make(map[string]bool)
	for _, name := range n.watcher.WatchList() {
		current[name] = true
		if !desired[name] {
			// The kernel drops the watch on a deleted directory by itself.
			// Dropping it again fails as unknown, or as invalid while the
			// watcher has not yet processed the deletion.
			if err := n.watcher.Remove(name); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) && !errors.Is(err, syscall.EINVAL) {
				return fmt.Errorf("stop watching %s: %w", name, err)
			}
		}
	}
	for name := range desired {
		if !current[name] {
			if err := n.watcher.Add(name); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("watch backing directory: %w", err)
			}
		}
	}
	return nil
}

func (n *notifications) relevant(event fsnotify.Event) bool {
	if event.Op == fsnotify.Chmod {
		return false
	}
	for _, root := range n.roots {
		base := nativePath(root.file)
		rel, err := filepath.Rel(base, event.Name)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		if !root.git {
			return true
		}
		if rel == "worktrees" || filepath.Dir(rel) == "worktrees" || strings.HasSuffix(rel, "/gitdir") || strings.HasSuffix(rel, "/locked") {
			return true
		}
	}
	return false
}

// unregistered reports that event ended a worktree registration and returns
// its name, or "" when the directory holding every registration went away.
func (n *notifications) unregistered(event fsnotify.Event) (name string, ended bool) {
	if !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
		return "", false
	}
	for _, root := range n.roots {
		if !root.git {
			continue
		}
		rel, err := filepath.Rel(nativePath(root.file), event.Name)
		if err != nil {
			continue
		}
		if rel == "worktrees" {
			return "", true
		}
		if filepath.Dir(rel) == "worktrees" {
			return filepath.Base(rel), true
		}
	}
	return "", false
}

func (n *notifications) close() error {
	err := n.watcher.Close()
	for _, root := range n.roots {
		err = errors.Join(err, root.file.Close())
	}
	return err
}
