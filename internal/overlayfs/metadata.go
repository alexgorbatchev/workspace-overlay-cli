package overlayfs

import (
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
)

const metadataTimeout = time.Second
const metadataWatchLimit = 256
const metadataEntryLimit = 1024

// metadataFS implements per-result negative caching. go-fuse's inode bridge
// only converts ENOENT to a negative entry when its global NegativeTimeout is
// set. A global timeout would also cache Git's hidden overlay-only files.
type metadataFS struct{ fuse.RawFileSystem }

func (m *metadataFS) Lookup(cancel <-chan struct{}, header *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	status := m.RawFileSystem.Lookup(cancel, header, name, out)
	if status == fuse.ENOENT && out.EntryTimeout() > 0 {
		return fuse.OK // NodeId zero is the FUSE protocol's negative entry.
	}
	return status
}

// projectNamespace proves overlays cannot ever contribute at name. The
// literal prefix is a conservative bound, not a second glob implementation:
// uncertain patterns (including escapes) keep the whole namespace uncached.
func (v *View) projectNamespace(name string) bool {
	if gitMetadata(name) {
		return true
	}
	for _, layer := range v.layers[1:] {
		if layer.rules == nil {
			return false
		}
		glob := layer.rules.glob
		if i := strings.IndexAny(glob, "*?[{\\"); i >= 0 {
			glob = glob[:i]
			if i := strings.LastIndexByte(glob, '/'); i >= 0 {
				glob = glob[:i]
			} else {
				return false
			}
		}
		glob = path.Clean(glob)
		if name == "." || glob == "." || name == glob || strings.HasPrefix(name, glob+"/") || strings.HasPrefix(glob, name+"/") {
			return false
		}
	}
	return true
}

type metadataDirectory struct {
	file    *os.File
	nodes   map[*fs.Inode]bool
	dead    bool
	entries map[string]time.Time
}

// Watches are acquired lazily through native backing handles, never by walking
// the mounted project. The cap bounds both descriptors and inotify watches;
// directories beyond it retain zero timeouts. Close joins the owned worker.
type metadataCache struct {
	view     *View
	watcher  *fsnotify.Watcher
	mu       sync.Mutex
	dirs     map[*fs.Inode]*metadataDirectory
	paths    map[string]*metadataDirectory
	backings map[identity]*metadataDirectory
	failed   bool
	flush    chan struct{}
	done     chan struct{}
}

func newMetadataCache(v *View) (*metadataCache, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	c := &metadataCache{view: v, watcher: w, dirs: make(map[*fs.Inode]*metadataDirectory), paths: make(map[string]*metadataDirectory), backings: make(map[identity]*metadataDirectory), flush: make(chan struct{}, 1), done: make(chan struct{})}
	go c.run()
	return c, nil
}

func (c *metadataCache) watch(n *node, entry string) bool {
	if c == nil || n.Operations() == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return false
	}
	if dir := c.dirs[&n.Inode]; dir != nil {
		return !dir.dead && dir.remember(entry)
	}
	if len(c.dirs) >= metadataWatchLimit {
		return false
	}
	f, err := c.view.layers[0].root.Open(n.relativePath())
	if err != nil {
		return false // Overlay-only parents have no cacheable project entries.
	}
	info, err := f.Stat()
	if err != nil {
		logged.Close(f)
		return false
	}
	key := fileIdentity(0, info)
	if dir := c.backings[key]; dir != nil {
		// inotify coalesces watches of the same native directory. Keep
		// every FUSE alias on its one watch, including after layer changes.
		logged.Close(f)
		dir.nodes[&n.Inode] = true
		c.dirs[&n.Inode] = dir
		return !dir.dead && dir.remember(entry)
	}
	name := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	if err := c.watcher.Add(name); err != nil {
		logged.Close(f)
		return false
	}
	dir := &metadataDirectory{file: f, nodes: map[*fs.Inode]bool{&n.Inode: true}, entries: make(map[string]time.Time)}
	c.dirs[&n.Inode] = dir
	c.paths[name] = dir
	c.backings[key] = dir
	return dir.remember(entry)
}

// remember also records misses, which have no child inode to enumerate when
// RefreshPaths or a mutation flushes the cache.
func (d *metadataDirectory) remember(name string) bool {
	now := time.Now()
	if _, ok := d.entries[name]; ok {
		d.entries[name] = now.Add(metadataTimeout)
		return true
	}
	if len(d.entries) >= metadataEntryLimit {
		for name, expiry := range d.entries {
			if now.After(expiry) {
				delete(d.entries, name)
			}
		}
		if len(d.entries) >= metadataEntryLimit {
			return false
		}
	}
	d.entries[name] = now.Add(metadataTimeout)
	return true
}

func (v *View) invalidateMetadata() {
	if v.cache == nil {
		return
	}
	select {
	case v.cache.flush <- struct{}{}:
	default:
	}
}

func (c *metadataCache) notifyEntry(n *fs.Inode, name string) {
	if child := n.GetChild(name); child != nil {
		// Negative offset invalidates attributes without flushing file data.
		c.notification(child.NotifyContent(-1, 0))
	}
	c.notification(n.NotifyEntry(name))
}

func (c *metadataCache) notification(errno syscall.Errno) {
	if errno != 0 && errno != syscall.ENOENT && errno != syscall.ENODEV {
		c.disable(errno)
	}
}

func (c *metadataCache) disable(err error) {
	c.mu.Lock()
	first := !c.failed
	c.failed = true
	c.mu.Unlock()
	if first {
		log.Printf("disable metadata caching: %v", err)
		c.view.invalidateMetadata()
	}
}

func (c *metadataCache) invalidateAll() {
	c.mu.Lock()
	type entry struct {
		parent *fs.Inode
		name   string
	}
	var entries []entry
	for n, dir := range c.dirs {
		for name := range dir.entries {
			entries = append(entries, entry{n, name})
		}
	}
	c.mu.Unlock()
	for _, entry := range entries {
		c.notifyEntry(entry.parent, entry.name)
	}
}

func (c *metadataCache) event(event fsnotify.Event) {
	c.mu.Lock()
	dir := c.paths[path.Dir(event.Name)]
	if own := c.paths[event.Name]; own != nil {
		// A moved/deleted directory's watch no longer proves that pathname
		// is covered. Never reuse it for a later inode at the same path.
		if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
			own.dead = true
		}
		dir = own
	}
	var nodes []*fs.Inode
	if dir != nil {
		for n := range dir.nodes {
			nodes = append(nodes, n)
		}
	}
	c.mu.Unlock()
	if dir != nil {
		for _, n := range nodes {
			c.notification(n.NotifyContent(-1, 0))
			c.notifyEntry(n, path.Base(event.Name))
		}
		if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
			c.invalidateAll()
		}
	}
}

func (c *metadataCache) run() {
	defer close(c.done)
	for {
		select {
		case event, ok := <-c.watcher.Events:
			if !ok {
				return
			}
			c.event(event)
		case err, ok := <-c.watcher.Errors:
			if !ok {
				return
			}
			// Overflow or a broken watcher removes the freshness guarantee.
			// Stop issuing TTLs and invalidate everything already published.
			c.disable(err)
			c.invalidateAll()
		case <-c.flush:
			c.invalidateAll()
		}
	}
}

func (c *metadataCache) close() {
	if err := c.watcher.Close(); err != nil {
		log.Printf("close metadata watcher: %v", err)
	}
	<-c.done
	for _, dir := range c.paths {
		logged.Close(dir.file)
	}
}
