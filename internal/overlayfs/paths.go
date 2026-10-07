package overlayfs

import (
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
)

type pathRules struct {
	mu      sync.RWMutex
	glob    string
	visible map[string]bool
}

func gitMetadata(name string) bool { return name == ".git" || strings.HasPrefix(name, ".git/") }

func (l *layer) refreshPaths() error {
	if l.rules == nil {
		return nil
	}
	names, err := doublestar.Glob(l.root.FS(), l.rules.glob, doublestar.WithFailOnIOErrors(), doublestar.WithNoFollow())
	if err != nil {
		return err
	}
	visible := map[string]bool{".": true}
	for _, name := range names {
		if gitMetadata(name) {
			continue
		}
		for name != "." {
			visible[name] = true
			name = path.Dir(name)
		}
	}
	l.rules.mu.Lock()
	l.rules.visible = visible
	l.rules.mu.Unlock()
	return nil
}

func (l *layer) includes(name string) bool {
	if l.rules == nil {
		return true
	}
	l.rules.mu.RLock()
	defer l.rules.mu.RUnlock()
	return l.rules.visible[name]
}

// RefreshPaths re-reads which overlay paths are visible and brings the Git exclusions up to date.
func (v *View) RefreshPaths() error {
	for i := range v.layers {
		if err := v.layers[i].refreshPaths(); err != nil {
			return err
		}
	}
	v.excludeClean.Store(false)
	return v.refreshExclude()
}

// overlays reports whether an overlay layer provides name.
func (v *View) overlays(name string) bool {
	if gitMetadata(name) {
		return false
	}
	for i := range v.layers[1:] {
		if v.layers[i+1].includes(name) {
			return true
		}
	}
	return false
}

// changed brings the view up to date after a mutation in layer index touched
// the given paths. A project change matters only where an overlay provides
// the same path, because that decides whether Git must ignore it.
func (v *View) changed(index int, names ...string) error {
	if index > 0 {
		return v.RefreshPaths()
	}
	if slices.ContainsFunc(names, v.overlays) {
		v.excludeClean.Store(false)
		return v.refreshExclude()
	}
	return nil
}
