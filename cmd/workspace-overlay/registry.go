package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

type recordedMount struct {
	Target  string `json:"target"`
	Exclude string `json:"exclude,omitempty"`
	Block   string `json:"block,omitempty"`
}

type mountState struct {
	Version int             `json:"version"`
	Project string          `json:"project"`
	Mounts  []recordedMount `json:"mounts"`
}

type mountRegistry struct {
	mu              sync.Mutex
	dir, file, stop string
	lock            *os.File
	state           mountState
}

func registryPaths(root, project string) (dir, file, stop string) {
	dir = filepath.Join(root, ".tmp", "workspace-overlay")
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(project)))
	file = filepath.Join(dir, key+".json")
	stop = filepath.Join(dir, key+".stop")
	return
}

func readRegistry(root, project string) (mountState, error) {
	_, file, _ := registryPaths(root, project)
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return mountState{}, nil
	}
	if err != nil {
		return mountState{}, err
	}
	var state mountState
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("read mount registry: %w", err)
	}
	if state.Version != 1 || state.Project != project {
		return state, fmt.Errorf("invalid mount registry %s", file)
	}
	for _, mount := range state.Mounts {
		if !filepath.IsAbs(mount.Target) || (mount.Exclude != "" && !filepath.IsAbs(mount.Exclude)) {
			return state, fmt.Errorf("registry paths must be absolute")
		}
	}
	return state, nil
}

func openRegistry(root, project string) (*mountRegistry, error) {
	r, err := acquireRegistry(root, project)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(r.file); !os.IsNotExist(err) {
		closeFile(r.lock)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("project %s has an unfinished registry; use overlay unmount or mount --replace to recover", project)
	}
	return r, nil
}

func acquireRegistry(root, project string) (*mountRegistry, error) {
	dir, file, stop := registryPaths(root, project)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(file+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeFile(lock)
		return nil, fmt.Errorf("project %s already has a running overlay process: %w", project, err)
	}
	registry := &mountRegistry{dir: dir, file: file, stop: stop, lock: lock, state: mountState{Version: 1, Project: project}}
	return registry, nil
}

func (r *mountRegistry) saveLocked() error {
	data, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(r.file+".next", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		closeFile(file)
		return err
	}
	if err := file.Sync(); err != nil {
		closeFile(file)
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(r.file+".next", r.file)
}

func (r *mountRegistry) record(target, exclude string, block []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	mount := recordedMount{Target: target, Exclude: exclude, Block: string(block)}
	for i := range r.state.Mounts {
		if r.state.Mounts[i].Target == target {
			r.state.Mounts[i] = mount
			return r.saveLocked()
		}
	}
	r.state.Mounts = append(r.state.Mounts, mount)
	sort.Slice(r.state.Mounts, func(i, j int) bool { return r.state.Mounts[i].Target < r.state.Mounts[j].Target })
	return r.saveLocked()
}

func (r *mountRegistry) forget(target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, mount := range r.state.Mounts {
		if mount.Target == target {
			r.state.Mounts = append(r.state.Mounts[:i], r.state.Mounts[i+1:]...)
			break
		}
	}
	return r.saveLocked()
}

func removeOwned(name string) error {
	err := os.Remove(name)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (r *mountRegistry) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.state.Mounts) != 0 {
		return errors.Join(fmt.Errorf("cleanup incomplete; mount registry retained for overlay unmount recovery"), r.lock.Close())
	}
	return errors.Join(removeOwned(r.file), removeOwned(r.file+".next"), removeOwned(r.stop), r.lock.Close())
}

func stopRegistered(ctx context.Context, root, project string) error {
	state, err := readRegistry(root, project)
	if err != nil || state.Version == 0 {
		return err
	}
	_, file, stop := registryPaths(root, project)
	if err := os.WriteFile(stop, nil, 0600); err != nil {
		return err
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-deadline.C:
			return recoverRegistry(ctx, root, project)
		}
	}
}

func recoverRegistry(ctx context.Context, root, project string) error {
	r, err := acquireRegistry(root, project)
	if err != nil {
		return err
	}
	defer closeFile(r.lock)
	state, err := readRegistry(root, project)
	if err != nil {
		return err
	}
	r.state = state
	for _, mount := range state.Mounts {
		if err := unmountOverlay(ctx, mount.Target); err != nil {
			return err
		}
	}
	for _, mount := range state.Mounts {
		if mount.Exclude != "" && mount.Block != "" {
			backing, err := os.OpenRoot(filepath.Dir(mount.Exclude))
			if err != nil {
				return err
			}
			g := &gitExclude{root: backing, file: mount.Exclude, block: []byte(mount.Block)}
			if err := g.close(); err != nil {
				return fmt.Errorf("recover Git exclusions: %w", err)
			}
		}
		if err := r.forget(mount.Target); err != nil {
			return err
		}
	}
	return errors.Join(removeOwned(r.file), removeOwned(r.stop), removeOwned(r.file+".next"))
}
