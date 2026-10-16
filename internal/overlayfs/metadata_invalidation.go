package overlayfs

import (
	"errors"
	"path"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
)

type metadataEntry struct {
	parent *fs.Inode
	name   string
}

// Data pages outlive entry TTLs. Include live cached inodes even after their
// names have expired from the bounded metadata bookkeeping.
func cachedEntries(n *fs.Inode, dir *metadataDirectory) []metadataEntry {
	entries := make([]metadataEntry, 0, len(dir.entries))
	for name := range dir.entries {
		entries = append(entries, metadataEntry{n, name})
	}
	for name, child := range n.Children() {
		if _, tracked := dir.entries[name]; !tracked && child.Operations().(*node).dataCached.Load() {
			entries = append(entries, metadataEntry{n, name})
		}
	}
	return entries
}

// invalidatePaths runs on the owned worker, outside filesystem request locks.
// Directory mutations invalidate their tracked descendants as well as their
// parent entry; sibling directories retain their cached entries and pages.
func (c *metadataCache) invalidatePaths(names []string) {
	c.mu.Lock()
	var entries []metadataEntry
	for n, dir := range c.dirs {
		parent := n.Operations().(*node).relativePath()
		for _, name := range names {
			if parent == path.Dir(name) {
				entries = append(entries, metadataEntry{n, path.Base(name)})
			}
			if parent == name || strings.HasPrefix(parent, name+"/") {
				entries = append(entries, cachedEntries(n, dir)...)
			}
		}
	}
	c.mu.Unlock()
	for _, entry := range entries {
		c.notifyEntry(entry.parent, entry.name)
	}
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
	var entries []metadataEntry
	for n := range oldest.nodes {
		delete(c.dirs, n)
		entries = append(entries, cachedEntries(n, oldest)...)
	}
	delete(c.paths, oldest.name)
	delete(c.backings, oldest.key)
	c.mu.Unlock()
	for _, entry := range entries {
		c.notifyEntry(entry.parent, entry.name)
	}
	logged.Close(oldest.file)
}
