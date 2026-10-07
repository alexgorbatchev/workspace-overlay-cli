// Package pathname names filesystem paths for people and compares them.
package pathname

import (
	"os"
	"path/filepath"
	"strings"
)

// Display abbreviates a path inside the user's home directory with "~".
func Display(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return name
	}
	rel, err := filepath.Rel(home, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return name
	}
	if rel == "." {
		return "~"
	}
	return "~" + string(filepath.Separator) + rel
}

// Contains reports whether child is parent itself or lies beneath it.
func Contains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Canonical resolves symbolic links so every command names a path the way
// Git and the mount table do. Paths that do not exist yet are only cleaned.
func Canonical(name string) string {
	if resolved, err := filepath.EvalSymlinks(name); err == nil {
		return resolved
	}
	return filepath.Clean(name)
}
