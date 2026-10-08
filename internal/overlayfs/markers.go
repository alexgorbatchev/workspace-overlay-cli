package overlayfs

import (
	"bytes"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bmatcuk/doublestar/v4"
)

// MarkerRule defines start and end markers for a file glob.
type MarkerRule struct {
	Glob  string
	Start string
	End   string
}

var defaultMarkerRules = []MarkerRule{
	{
		Glob:  "*.{md,markdown}",
		Start: "<!-- BEGIN WORKSPACE-OVERLAY: {name} ({path}) -->",
		End:   "<!-- END WORKSPACE-OVERLAY: {name} -->",
	},
	{
		Glob:  "*.{html,htm,xml,svg}",
		Start: "<!-- BEGIN WORKSPACE-OVERLAY: {name} ({path}) -->",
		End:   "<!-- END WORKSPACE-OVERLAY: {name} -->",
	},
	{
		Glob:  "*.{toml,yaml,yml,sh,bash,zsh,py,rb,env,env.*,*ignore,Dockerfile*,Makefile*}",
		Start: "# BEGIN WORKSPACE-OVERLAY: {name} ({path})",
		End:   "# END WORKSPACE-OVERLAY: {name}",
	},
	{
		Glob:  "*.{go,ts,js,tsx,jsx,rs,c,cpp,h,hpp,java,css,scss,less}",
		Start: "// BEGIN WORKSPACE-OVERLAY: {name} ({path})",
		End:   "// END WORKSPACE-OVERLAY: {name}",
	},
}

// SetMarkers attaches custom marker rules to the view.
func (v *View) SetMarkers(rules []MarkerRule) {
	v.markers = rules
}

// findMarkerRule returns the first marker rule matching path, or nil.
func (v *View) findMarkerRule(path string) *MarkerRule {
	if len(v.markers) == 0 {
		return nil
	}
	base := filepath.Base(path)
	for _, rule := range v.markers {
		if matched, _ := doublestar.Match(rule.Glob, base); matched {
			return &rule
		}
		if matched, _ := doublestar.Match(rule.Glob, path); matched {
			return &rule
		}
	}
	return nil
}

func formatMarker(template, name, relPath string) string {
	r := strings.ReplaceAll(template, "{name}", name)
	return strings.ReplaceAll(r, "{path}", relPath)
}

func markerPrefix(template, name string) string {
	idx := strings.Index(template, "{name}")
	if idx >= 0 {
		return template[:idx] + name
	}
	return template
}

// parsedSection holds one parsed layer contribution from a marked document.
type parsedSection struct {
	index int
	data  []byte
}

// parseMarkedDocument splits data into section 0 (base) and sections for each overlay part.
// Returns syscall.EPERM if markers are missing, corrupted, or out of order.
func parseMarkedDocument(data []byte, parts []contribution, layers []layer, rule *MarkerRule) ([]parsedSection, error) {
	if len(parts) <= 1 {
		return []parsedSection{{index: parts[0].index, data: data}}, nil
	}

	type markerSpan struct {
		name                 string
		startPos, startEnd   int
		endPos, endEnd       int
		contentStart, contentEnd int
	}

	spans := make([]markerSpan, len(parts)-1)
	searchFrom := 0

	for i := 1; i < len(parts); i++ {
		name := layers[parts[i].index].name
		if name == "" {
			name = filepath.Base(layers[parts[i].index].root.Name())
		}
		spans[i-1].name = name

		startMarkerTag := []byte(markerPrefix(rule.Start, name))
		idx := bytes.Index(data[searchFrom:], startMarkerTag)
		if idx < 0 {
			return nil, syscall.EPERM
		}
		startPos := searchFrom + idx
		lineEnd := bytes.IndexByte(data[startPos:], '\n')
		if lineEnd < 0 {
			return nil, syscall.EPERM
		}
		spans[i-1].startPos = startPos
		spans[i-1].startEnd = startPos + lineEnd + 1
		spans[i-1].contentStart = spans[i-1].startEnd

		endMarkerTag := []byte(markerPrefix(rule.End, name))
		endIdx := bytes.Index(data[spans[i-1].contentStart:], endMarkerTag)
		if endIdx < 0 {
			return nil, syscall.EPERM
		}
		endPos := spans[i-1].contentStart + endIdx
		endLineEnd := bytes.IndexByte(data[endPos:], '\n')
		if endLineEnd < 0 {
			endLineEnd = len(data) - endPos
		} else {
			endLineEnd++
		}
		spans[i-1].endPos = endPos
		spans[i-1].endEnd = endPos + endLineEnd
		spans[i-1].contentEnd = endPos

		searchFrom = spans[i-1].endEnd
	}

	// Section 0 is before the first marker.
	sec0 := data[:spans[0].startPos]
	result := make([]parsedSection, len(parts))
	result[0] = parsedSection{index: parts[0].index, data: bytes.Clone(sec0)}

	for i := 1; i < len(parts); i++ {
		secData := data[spans[i-1].contentStart:spans[i-1].contentEnd]
		result[i] = parsedSection{index: parts[i].index, data: bytes.Clone(secData)}
	}

	return result, nil
}
