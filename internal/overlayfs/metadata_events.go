package overlayfs

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

const metadataEventLimit = 4096

// The collector never takes cache/request locks or calls into FUSE. Repeated
// events combine by pathname; exhausting this bound triggers the same recovery
// as kernel event loss instead of blocking intake or growing without limit.
type metadataInbox struct {
	mu     sync.Mutex
	paused atomic.Bool
	events map[string]fsnotify.Op
	lost   bool
	err    error
}

func (q *metadataInbox) add(event fsnotify.Event, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if errors.Is(err, fsnotify.ErrEventOverflow) {
		q.lost = true
	} else if err != nil {
		q.err = err
	}
	if event.Op != 0 && !q.lost && q.err == nil {
		if _, exists := q.events[event.Name]; !exists && len(q.events) >= metadataEventLimit {
			q.lost = true
		} else {
			if q.events == nil {
				q.events = make(map[string]fsnotify.Op)
			}
			q.events[event.Name] |= event.Op
		}
	}
	if q.lost || q.err != nil {
		q.paused.Store(true)
		q.events = nil
	}
}

func (c *metadataCache) collect(w *fsnotify.Watcher) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				c.inbox.add(event, nil)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				c.inbox.add(fsnotify.Event{}, err)
			}
			select {
			case c.wake <- struct{}{}:
			default:
			}
		}
	}()
	return done
}

func (c *metadataCache) stopCollector() {
	if err := c.watcher.Close(); err != nil {
		log.Printf("close metadata watcher: %v", err)
	}
	<-c.collected
}

func (c *metadataCache) processEvents() {
	q := &c.inbox
	q.mu.Lock()
	events, lost, err := q.events, q.lost, q.err
	q.events, q.lost, q.err = nil, false, nil
	q.mu.Unlock()
	if err != nil {
		c.disable(err)
		c.invalidateAll()
		return
	}
	if lost {
		c.recoverEvents()
		return
	}
	for name, op := range events {
		c.event(fsnotify.Event{Name: name, Op: op})
	}
}

// Event loss can hide watch deletion and directory moves, not just writes.
// Replace inotify's bookkeeping, rearm native handles, and verify pathname
// identities before allowing cache reuse. Watch before invalidating: even held
// descriptors need subsequent writes delivered without another open/lookup.
func (c *metadataCache) recoverEvents() {
	c.stopCollector()
	q := &c.inbox
	q.mu.Lock()
	q.events, q.lost = nil, false // The retired watcher's remaining events.
	q.mu.Unlock()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		c.disable(err)
		c.invalidateAll()
		return
	}
	c.mu.Lock()
	c.watcher = w
	c.collected = c.collect(w)
	for _, dir := range c.paths {
		if err = w.Add(dir.name); err != nil {
			break
		}
		for n := range dir.nodes {
			info, statErr := c.view.Project().Stat(n.Operations().(*node).relativePath())
			if statErr != nil || fileIdentity(0, info) != dir.key {
				dir.dead = true
			}
		}
	}
	c.mu.Unlock()
	if err != nil {
		c.disable(err)
	}
	c.view.sniffs.clear()
	c.invalidateAll()
	q.mu.Lock()
	if !q.lost && q.err == nil {
		q.paused.Store(false)
	}
	q.mu.Unlock()
}
