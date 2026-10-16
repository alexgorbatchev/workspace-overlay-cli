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

func (l *layer) refreshPaths() ([]string, error) {
	if l.rules == nil {
		return nil, nil
	}
	names, err := doublestar.Glob(l.root.FS(), l.rules.glob, doublestar.WithFailOnIOErrors(), doublestar.WithNoFollow())
	if err != nil {
		return nil, err
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
	// Reconciliation can change contents as well as membership. Cover both
	// snapshots, but only within the overlay's populated top-level subtrees.
	// Project-only siblings keep their cached entries and file pages.
	touched := make(map[string]bool)
	for _, paths := range []map[string]bool{l.rules.visible, visible} {
		for name := range paths {
			if name != "." && !strings.ContainsRune(name, '/') {
				touched[name] = true
			}
		}
	}
	l.rules.visible = visible
	l.rules.mu.Unlock()
	changed := make([]string, 0, len(touched))
	for name := range touched {
		changed = append(changed, name)
	}
	return changed, nil
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
	var changed []string
	for i := range v.layers {
		names, err := v.layers[i].refreshPaths()
		if err != nil {
			v.invalidateMetadata() // A partial refresh cannot identify every change.
			return err
		}
		changed = append(changed, names...)
	}
	defer v.invalidatePaths(changed...)
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
	defer v.invalidatePaths(names...)
	if slices.ContainsFunc(names, v.overlays) {
		v.excludeClean.Store(false)
		return v.refreshExclude()
	}
	return nil
}
