// Package registry records which mounts an owner process holds, so another
// process can ask it to stop or clean up after it.
package registry

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
	"workspace-overlay/internal/logged"
)

// Mount records one mounted overlay with its Git exclusions.
type Mount struct {
	Target  string `json:"target"`
	Exclude string `json:"exclude,omitempty"`
	Block   string `json:"block,omitempty"`
}

// State is the complete record of mounts held by one owner process.
type State struct {
	Version int     `json:"version"`
	Root    string  `json:"root"`
	Project string  `json:"project"`
	Mounts  []Mount `json:"mounts"`
}

type Registry struct {
	mu              sync.Mutex
	dir, file, stop string
	lock            *os.File
	state           State
}

// stateDir is where mount registries live, following the XDG Base Directory
// specification: a relative XDG_STATE_HOME is invalid and ignored.
func stateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "workspace-overlay"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "workspace-overlay"), nil
}

// Paths returns the state directory, registry file, and stop-request file for a project.
func Paths(root, project string) (dir, file, stop string, err error) {
	dir, err = stateDir()
	if err != nil {
		return "", "", "", err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(root+"\x00"+project)))
	file = filepath.Join(dir, key+".json")
	stop = filepath.Join(dir, key+".stop")
	return
}

// Read returns the recorded state for a project, or an empty state if no record exists.
func Read(root, project string) (State, error) {
	_, file, _, err := Paths(root, project)
	if err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("read mount registry: %w", err)
	}
	if state.Version != 1 || state.Project != project || state.Root != root {
		return state, fmt.Errorf("invalid mount registry %s", file)
	}
	for _, mount := range state.Mounts {
		if !filepath.IsAbs(mount.Target) || (mount.Exclude != "" && !filepath.IsAbs(mount.Exclude)) {
			return state, fmt.Errorf("registry paths must be absolute")
		}
	}
	return state, nil
}

// Open returns a new registry handle for a project with no existing unfinished mounts.
func Open(root, project string) (*Registry, error) {
	r, err := Acquire(root, project)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(r.file); !os.IsNotExist(err) {
		logged.Close(r.lock)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("project %s has an unfinished registry; use overlay unmount or mount --replace to recover", project)
	}
	if err := removeOwned(r.stop); err != nil {
		logged.Close(r.lock)
		return nil, err
	}
	return r, nil
}

// Acquire returns a new registry handle, acquiring the lock even if a record exists (for recovery).
func Acquire(root, project string) (*Registry, error) {
	dir, file, stop, err := Paths(root, project)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(file+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		logged.Close(lock)
		return nil, fmt.Errorf("project %s already has a running overlay process: %w", project, err)
	}
	registry := &Registry{dir: dir, file: file, stop: stop, lock: lock, state: State{Version: 1, Root: root, Project: project}}
	return registry, nil
}

// saveLocked must be called while holding r.mu.
func (r *Registry) saveLocked() error {
	data, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(r.file+".next", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		logged.Close(file)
		return err
	}
	if err := file.Sync(); err != nil {
		logged.Close(file)
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(r.file+".next", r.file)
}

// Record adds or updates a mount in the registry.
func (r *Registry) Record(target, exclude string, block []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	mount := Mount{Target: target, Exclude: exclude, Block: string(block)}
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

// Forget removes a mount from the registry.
func (r *Registry) Forget(target string) error {
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

// Dir returns the directory watched for stop requests.
func (r *Registry) Dir() string {
	return r.dir
}

// File returns the path to the registry record file.
func (r *Registry) File() string {
	return r.file
}

// StopFile returns the path to the stop-request file.
func (r *Registry) StopFile() string {
	return r.stop
}

// Adopt takes over the records of an owner that is gone, so they can be cleaned up one by one.
func (r *Registry) Adopt(state State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = state
}

// Discard removes the record, stop-request and staging files after every mount has been cleaned up.
func (r *Registry) Discard() error {
	return errors.Join(removeOwned(r.file), removeOwned(r.stop), removeOwned(r.file+".next"))
}

// Unlock releases ownership and keeps the records, for example after a recovery attempt.
func (r *Registry) Unlock() {
	logged.Close(r.lock)
}

// Close removes the record if cleanup is complete, or keeps it for recovery.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.state.Mounts) != 0 {
		return errors.Join(fmt.Errorf("cleanup incomplete; mount registry retained for overlay unmount recovery"), r.lock.Close())
	}
	return errors.Join(removeOwned(r.file), removeOwned(r.file+".next"), removeOwned(r.stop), r.lock.Close())
}

func removeOwned(name string) error {
	err := os.Remove(name)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RequestStop asks the owner of a project's mounts to stop and waits for it to
// release its records. It reports false when no owner answered in time: the owner
// is gone and its records need recovery.
func RequestStop(ctx context.Context, root, project string) (released bool, err error) {
	// The stop file takes effect as soon as it exists, so a request that is
	// already canceled must not write it.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	state, err := Read(root, project)
	if err != nil || state.Version == 0 {
		return err == nil, err
	}
	// A live owner holds the lock. If it can be taken, the owner is gone:
	// nobody would answer a stop request, so report that without waiting.
	if abandoned, err := Acquire(root, project); err == nil {
		abandoned.Unlock()
		return false, nil
	}
	_, file, stop, err := Paths(root, project)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(stop, nil, 0600); err != nil {
		return false, err
	}

	const pollInterval = 25 * time.Millisecond
	const waitTimeout = 3 * time.Second

	deadline := time.NewTimer(waitTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			return true, removeOwned(stop)
		} else if err != nil {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		case <-deadline.C:
			return false, nil
		}
	}
}
