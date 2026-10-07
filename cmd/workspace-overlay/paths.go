package main

import (
	"path"
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

func (v *view) refreshPaths() error {
	for i := range v.layers {
		if err := v.layers[i].refreshPaths(); err != nil {
			return err
		}
	}
	return v.refreshExclude()
}
