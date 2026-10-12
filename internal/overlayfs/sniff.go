package overlayfs

import (
	"container/list"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
)

const sniffFileLimit = 64

type sniffKey struct {
	layer int
	name  string
}

type sniffFile struct {
	mu    sync.RWMutex // Readers pin the descriptor across eviction and invalidation.
	key   sniffKey
	file  *os.File
	stat  syscall.Stat_t
	entry *list.Element
}

// sniffCache retains bounded read descriptors, never file content. Every
// classification reads the requested bytes again. The resolved inode and ctime
// prevent reuse after replacement, content changes, or permission/ACL changes.
type sniffCache struct {
	mu     sync.Mutex
	files  map[sniffKey]*sniffFile
	order  list.List
	closed bool
}

func (c *sniffCache) read(root *os.Root, key sniffKey, info os.FileInfo, head []byte, off int64) (int, error) {
	f, err := c.acquire(root, key, info.Sys().(*syscall.Stat_t))
	if err != nil {
		return 0, err
	}
	n, err := f.file.ReadAt(head, off)
	f.mu.RUnlock()
	if err == io.EOF {
		err = nil // The sniffed window includes the entire short file.
	}
	if err != nil {
		c.mu.Lock()
		current := c.files[key] == f
		if current {
			c.detach(f)
		}
		c.mu.Unlock()
		if current {
			f.close()
		}
	}
	return n, err
}

func (c *sniffCache) acquire(root *os.Root, key sniffKey, stat *syscall.Stat_t) (*sniffFile, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, os.ErrClosed
	}
	f := c.files[key]
	if f != nil && f.matches(stat) {
		c.pin(f)
		c.mu.Unlock()
		return f, nil
	}
	if f != nil {
		c.detach(f)
	}
	c.mu.Unlock()
	if f != nil {
		f.close()
	}
	// Cold reads must retain native filesystem concurrency. Regular-file
	// reads ignore O_NONBLOCK; it avoids Go's epoll setup.
	file, err := root.OpenFile(key.name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		logged.Close(file)
		return nil, os.ErrClosed
	}
	if f = c.files[key]; f != nil && f.matches(stat) {
		c.pin(f)
		c.mu.Unlock()
		logged.Close(file) // Another reader populated the entry while we opened it.
		return f, nil
	}
	if f == nil && len(c.files) == sniffFileLimit {
		f = c.order.Back().Value.(*sniffFile)
	}
	if f != nil {
		c.detach(f)
	}
	if c.files == nil {
		c.files = make(map[sniffKey]*sniffFile)
	}
	opened := &sniffFile{key: key, file: file, stat: *stat}
	opened.entry = c.order.PushFront(opened)
	c.files[key] = opened
	opened.mu.RLock()
	c.mu.Unlock()
	if f != nil {
		f.close()
	}
	return opened, nil
}

func (f *sniffFile) matches(stat *syscall.Stat_t) bool {
	return f.stat.Dev == stat.Dev && f.stat.Ino == stat.Ino && f.stat.Ctim == stat.Ctim && f.stat.Mode == stat.Mode && f.stat.Uid == stat.Uid && f.stat.Gid == stat.Gid
}

// pin is called under the cache lock, before an entry can be detached.
func (c *sniffCache) pin(f *sniffFile) {
	c.order.MoveToFront(f.entry)
	f.mu.RLock()
}

func (c *sniffCache) detach(f *sniffFile) {
	delete(c.files, f.key)
	c.order.Remove(f.entry)
}

func (f *sniffFile) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	logged.Close(f.file)
}

func (c *sniffCache) clear() {
	c.clearFiles(false)
}

func (c *sniffCache) close() {
	c.clearFiles(true)
}

func (c *sniffCache) clearFiles(closing bool) {
	c.mu.Lock()
	c.closed = c.closed || closing
	files := c.files
	c.files = nil
	c.order.Init()
	c.mu.Unlock()
	for _, f := range files {
		f.close()
	}
}
