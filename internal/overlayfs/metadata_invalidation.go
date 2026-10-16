package overlayfs

import (
	"errors"
	"path"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
)

type metadataInvalidation struct {
	inode *fs.Inode
	name  string // Empty invalidates the inode's attributes, otherwise its named entry.
}

// Data pages outlive entry TTLs. Include live cached inodes even after their
// names have expired from the bounded metadata bookkeeping.
func cachedInvalidations(n *fs.Inode, dir *metadataDirectory) []metadataInvalidation {
	entries := make([]metadataInvalidation, 0, len(dir.entries)+1)
	entries = append(entries, metadataInvalidation{inode: n})
	for name := range dir.entries {
		entries = append(entries, metadataInvalidation{n, name})
	}
	for name, child := range n.Children() {
		if _, tracked := dir.entries[name]; !tracked && child.Operations().(*node).dataCached.Load() {
			entries = append(entries, metadataInvalidation{n, name})
		}
	}
	return entries
}

func (c *metadataCache) invalidate(entries []metadataInvalidation) {
	for _, entry := range entries {
		if entry.name == "" {
			c.notification(entry.inode.NotifyContent(-1, 0))
		} else {
			c.notifyEntry(entry.inode, entry.name)
		}
	}
}

// invalidatePaths runs on the owned worker, outside filesystem request locks.
// Directory mutations invalidate their tracked descendants as well as their
// parent entry; sibling directories retain their cached entries and pages.
func (c *metadataCache) invalidatePaths(names []string) {
	c.mu.Lock()
	var entries []metadataInvalidation
	for n, dir := range c.dirs {
		parent := n.Operations().(*node).relativePath()
		for _, name := range names {
			if parent == path.Dir(name) {
				entries = append(entries, metadataInvalidation{inode: n}, metadataInvalidation{n, path.Base(name)})
			}
			if parent == name || strings.HasPrefix(parent, name+"/") {
				entries = append(entries, cachedInvalidations(n, dir)...)
			}
		}
	}
	c.mu.Unlock()
	c.invalidate(entries)
}

// evictOldest recycles watch capacity instead of permanently excluding every
// directory first accessed after the cap. Retire before notifying so requests
// cannot reuse the evicted watch. Notifications may wait for an active FUSE
// operation, which is why eviction belongs to the worker, never Lookup.
func (c *metadataCache) evictOldest() {
	c.mu.Lock()
	var oldest *metadataDirectory
	for _, dir := range c.backings {
		if oldest == nil || dir.used.Before(oldest.used) {
			oldest = dir
		}
	}
	if oldest == nil {
		c.mu.Unlock()
		return
	}
	// Remove before publishing the free slot. Otherwise a concurrent watch
	// of this same backing directory can coalesce with the retiring watch,
	// and removing the old watch would silently remove the new one too.
	if err := c.watcher.Remove(oldest.name); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
		c.mu.Unlock()
		c.disable(err)
		return
	}
	oldest.dead = true
	var entries []metadataInvalidation
	for n := range oldest.nodes {
		delete(c.dirs, n)
		entries = append(entries, cachedInvalidations(n, oldest)...)
	}
	delete(c.paths, oldest.name)
	delete(c.backings, oldest.key)
	c.mu.Unlock()
	c.invalidate(entries)
	logged.Close(oldest.file)
}
