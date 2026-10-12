package overlayfs

import (
	"fmt"
	"testing"
)

func TestMarkerDecisionsRefreshAndStayBounded(t *testing.T) {
	var v View
	rules := []MarkerRule{
		{Glob: "notes/*.md", Start: "first", End: "end"},
		{Glob: "*.md", Start: "second", End: "end"},
	}
	v.SetMarkers(rules)
	check := func(name, start string) {
		t.Helper()
		rule := v.findMarkerRule(name)
		if start == "" {
			if rule != nil {
				t.Fatalf("%s: matched %+v, want no match", name, rule)
			}
		} else if rule == nil || rule.Start != start {
			t.Fatalf("%s: matched %+v, want %s", name, rule, start)
		}
	}
	for range 2 {
		check("notes/file.md", "first")
		check("other/file.md", "second")
		check("file.txt", "")
	}
	for i := range markerMatchLimit + 10 {
		check(fmt.Sprintf("file-%d.txt", i), "")
	}
	if len(v.matches.paths) != markerMatchLimit {
		t.Fatalf("retained %d marker decisions, want %d", len(v.matches.paths), markerMatchLimit)
	}
	// Both cached and uncached names remain correct at the capacity limit.
	check("notes/file.md", "first")
	check("new.md", "second")
	check("new.txt", "")
	v.SetMarkers([]MarkerRule{{Glob: "*.txt", Start: "replacement", End: "end"}})
	check("notes/file.md", "")
	check("file.txt", "replacement")
	check("new.txt", "replacement")
}
