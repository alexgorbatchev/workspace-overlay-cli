// Package gitexclude maintains one mount's managed block of patterns in a
// repository's info/exclude file.
package gitexclude

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/gitrepo"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/subprocess"
)

// File keeps a pre-mount handle: opening .git through the mounted filesystem would recurse.
type File struct {
	mu       sync.Mutex
	root     *os.Root
	path     string
	OnUpdate func([]byte) error
	marker   string
	block    []byte
}

// Open returns a gitexclude file handle for the project, or (nil, nil) if the project has no .git.
func Open(ctx context.Context, project string) (*File, error) {
	if _, err := os.Lstat(filepath.Join(project, ".git")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	cmd := gitrepo.Command(ctx, project, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	data, err := subprocess.Output(ctx, cmd)
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
	return &File{root: root, path: name, marker: fmt.Sprintf("workspace-overlay %x", rand.Text())}, nil
}

// Path returns the exclude file this block lives in.
func (f *File) Path() string {
	return f.path
}

// Update writes patterns to the managed block, replacing any previous patterns.
func (f *File) Update(patterns []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var block []byte
	if len(patterns) > 0 {
		block = []byte("\n# begin " + f.marker + "\n" + strings.Join(patterns, "\n") + "\n# end " + f.marker + "\n")
	}
	if bytes.Equal(block, f.block) {
		return nil
	}
	file, err := f.root.OpenFile("exclude", os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer logged.Close(file)
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	// Closing the descriptor releases the advisory lock.
	data, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	if len(f.block) > 0 {
		if bytes.Count(data, f.block) != 1 {
			return fmt.Errorf("managed Git exclude block changed; refusing to overwrite it")
		}
		data = bytes.Replace(data, f.block, nil, 1)
	}
	data = append(data, block...)
	if _, err := file.WriteAt(data, 0); err != nil {
		return err
	}
	if err := file.Truncate(int64(len(data))); err != nil {
		return err
	}
	f.block = block
	if err := file.Sync(); err != nil {
		return err
	}
	if f.OnUpdate != nil {
		return f.OnUpdate(block)
	}
	return nil
}

// Clone creates an independent copy of this File with a new marker and root handle.
func (f *File) Clone() (*File, error) {
	if f == nil {
		return nil, nil
	}
	root, err := f.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	return &File{root: root, path: f.path, marker: fmt.Sprintf("workspace-overlay %x", rand.Text())}, nil
}

// Close removes the managed block from the exclude file and closes the root handle.
func (f *File) Close() error {
	if f == nil {
		return nil
	}
	err := f.Update(nil)
	return errors.Join(err, f.root.Close())
}

// Pattern converts a path into a Git exclude pattern, escaping special characters.
func Pattern(name string) (string, error) {
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

// Restore removes a block recorded by a mount that ended without cleaning up.
func Restore(filePath string, block []byte) error {
	root, err := os.OpenRoot(filepath.Dir(filePath))
	if err != nil {
		return err
	}
	defer root.Close()
	f := &File{root: root, path: filePath, block: block}
	return f.Close()
}
