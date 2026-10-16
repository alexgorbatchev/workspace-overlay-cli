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

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fsnotify/fsnotify"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/logged"
)

const metadataTimeout = time.Second
const metadataWatchLimit = 256
const metadataEntryLimit = 1024
const metadataAttributeLimit = 64

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

// projectNamespace proves overlays cannot ever contribute at name, including
// as an ancestor directory of a future match. Match each component prefix
// with the same glob engine used to discover contributions. An incomplete
// alternative or escaped separator is uncertain and stays uncached.
func (v *View) projectNamespace(name string) bool {
	if gitMetadata(name) {
		return true
	}
	for _, layer := range v.layers[1:] {
		if layer.rules == nil {
			return false
		}
		if name == "." || strings.ContainsRune(layer.rules.glob, '\\') {
			return false
		}
		for glob := layer.rules.glob; ; glob = path.Dir(glob) {
			matched, err := doublestar.Match(glob, name)
			if matched || err != nil || glob == "." {
				return false
			}
			if !strings.ContainsRune(glob, '/') {
				break
			}
			if path.Dir(glob) == glob {
				return false
			}
		}
	}
	return true
}

type metadataDirectory struct {
	file    *os.File
	key     identity
	name    string
	used    time.Time
	nodes   map[*fs.Inode]bool
	dead    bool
	entries map[string]time.Time
}

// Watches are acquired lazily through native backing handles, never by walking
// the mounted project. The cap bounds both descriptors and inotify watches;
// directories beyond it retain zero timeouts. Close joins the owned worker.
type metadataCache struct {
	view      *View
	watcher   *fsnotify.Watcher
	mu        sync.Mutex
	dirs      map[*fs.Inode]*metadataDirectory
	paths     map[string]*metadataDirectory
	backings  map[identity]*metadataDirectory
	failed    bool
	inbox     metadataInbox
	wake      chan struct{}
	stop      chan struct{}
	collected chan struct{}
	flush     chan struct{}
	attrs     chan *fs.Inode
	changes   chan []string
	evict     chan struct{}
	done      chan struct{}
}

func newMetadataCache(v *View) (*metadataCache, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	c := &metadataCache{view: v, watcher: w, dirs: make(map[*fs.Inode]*metadataDirectory), paths: make(map[string]*metadataDirectory), backings: make(map[identity]*metadataDirectory), flush: make(chan struct{}, 1), attrs: make(chan *fs.Inode, metadataAttributeLimit), changes: make(chan []string, metadataAttributeLimit), evict: make(chan struct{}, 1), wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	c.collected = c.collect(w)
	go c.run()
	return c, nil
}

// An empty entry requests coverage for the directory's own attributes only.
func (c *metadataCache) watch(n *node, entry string) bool {
	if c == nil || n.Operations() == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed || c.inbox.paused.Load() {
		return false
	}
	if dir := c.dirs[&n.Inode]; dir != nil {
		dir.used = time.Now()
		return !dir.dead && dir.remember(entry)
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
		dir.used = time.Now()
		return !dir.dead && dir.remember(entry)
	}
	if len(c.backings) >= metadataWatchLimit {
		logged.Close(f)
		select {
		case c.evict <- struct{}{}:
		default:
		}
		return false
	}
	name := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	if err := c.watcher.Add(name); err != nil {
		logged.Close(f)
		return false
	}
	dir := &metadataDirectory{file: f, key: key, name: name, used: time.Now(), nodes: map[*fs.Inode]bool{&n.Inode: true}, entries: make(map[string]time.Time)}
	c.dirs[&n.Inode] = dir
	c.paths[name] = dir
	c.backings[key] = dir
	return dir.remember(entry)
}

// remember also records misses, which have no child inode to enumerate when
// RefreshPaths or a mutation flushes the cache.
func (d *metadataDirectory) remember(name string) bool {
	if name == "" {
		return true
	}
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
	v.sniffs.clear()
	if v.cache == nil {
		return
	}
	select {
	case v.cache.flush <- struct{}{}:
	default:
	}
}

func (v *View) invalidatePaths(names ...string) {
	v.sniffs.clear()
	if v.cache == nil || len(names) == 0 {
		return
	}
	select {
	case v.cache.changes <- names:
	default:
		v.invalidateMetadata()
	}
}

// Attribute mutations leave the inode's directory entries intact. Queue the
// notification on the owned worker, outside the FUSE request and its locks.
// A saturated queue falls back to a full flush rather than losing freshness.
func (v *View) invalidateAttributes(n *fs.Inode) {
	v.sniffs.clear()
	if v.cache == nil {
		return
	}
	select {
	case v.cache.attrs <- n:
	default:
		v.invalidateMetadata()
	}
}

func (c *metadataCache) notifyEntry(n *fs.Inode, name string) {
	if child := n.GetChild(name); child != nil {
		c.notification(child.NotifyContent(0, 0))
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
	var entries []metadataInvalidation
	for n, dir := range c.dirs {
		entries = append(entries, cachedInvalidations(n, dir)...)
	}
	c.mu.Unlock()
	c.invalidate(entries)
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
		name := path.Base(event.Name)
		for _, n := range nodes {
			if event.Name == dir.name {
				c.notification(n.NotifyContent(-1, 0))
				continue
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				c.notification(n.NotifyContent(-1, 0))
				c.notifyEntry(n, name)
				continue
			}
			child := n.GetChild(name)
			if child == nil || child.Mode()&syscall.S_IFMT != syscall.S_IFREG {
				continue
			}
			// Writes and permission changes preserve the directory entry.
			// Potential overlay paths retain zero attribute timeouts, but a
			// project-only open may have populated their data pages.
			if child.Operations().(*node).dataCached.Load() || c.view.projectNamespace(path.Join(n.Operations().(*node).relativePath(), name)) {
				offset := int64(-1) // chmod preserves cached data.
				if event.Has(fsnotify.Write) {
					offset = 0
				}
				c.notification(child.NotifyContent(offset, 0))
			}
		}
		if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
			c.view.sniffs.clear()
			// Only descendants of a moved directory lose path coverage.
			for _, n := range nodes {
				name := n.Operations().(*node).relativePath()
				if event.Name != dir.name {
					name = path.Join(name, path.Base(event.Name))
				}
				c.invalidatePaths([]string{name})
			}
		}
	}
}

func (c *metadataCache) run() {
	defer close(c.done)
	defer c.stopCollector()
	for {
		select {
		case <-c.stop:
			return
		case <-c.wake:
			c.processEvents()
		case <-c.flush:
			c.invalidateAll()
		case n := <-c.attrs:
			c.notification(n.NotifyContent(-1, 0))
		case names := <-c.changes:
			c.invalidatePaths(names)
		case <-c.evict:
			c.evictOldest()
		}
	}
}

func (c *metadataCache) close() {
	// Requests can still finish while a busy mount is being released. Stop
	// watch acquisition under its lock before joining the notification worker.
	c.mu.Lock()
	c.failed = true
	c.mu.Unlock()
	close(c.stop)
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, dir := range c.paths {
		logged.Close(dir.file)
	}
}
