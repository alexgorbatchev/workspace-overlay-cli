// Package config loads and validates workspace-overlay.toml.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/pelletier/go-toml/v2"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
)

// Name is the configuration file looked for in the working directory and its ancestors.
const Name = "workspace-overlay.toml"

type Options struct {
	Collision string `toml:"collision"`
	Write     string `toml:"write"`
}

type Project struct {
	Path string `toml:"path"`
}

type Overlay struct {
	Name      string   `toml:"name"`
	Source    string   `toml:"source"`
	Projects  []string `toml:"projects"`
	Glob      string   `toml:"glob"`
	Collision string   `toml:"collision"`
	Write     string   `toml:"write"`
}

// Configuration is the workspace overlay configuration loaded from workspace-overlay.toml.
type Configuration struct {
	Version  int                `toml:"version"`
	Defaults Options            `toml:"defaults"`
	Projects map[string]Project `toml:"projects"`
	Overlays []Overlay          `toml:"overlays"`
	root     string
}

// Source is one overlay directory that applies to a project, with the glob that selects its files.
type Source struct {
	Name, Path, Glob string
}

// Selection is one configured project with the overlay sources that apply to it.
type Selection struct {
	Root, Project, Target string
	Sources               []Source
}

// Path returns the canonical path to a configuration file, discovering it in the working
// directory and its ancestors when name is empty, or validating the provided path.
func Path(name string) (string, error) {
	if name != "" {
		abs, err := filepath.Abs(name)
		if err != nil {
			return "", err
		}
		return pathname.Canonical(abs), nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		name := filepath.Join(dir, Name)
		if _, err := os.Stat(name); err == nil {
			return pathname.Canonical(name), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%s not found; specify --config", Name)
		}
		dir = parent
	}
}

// Load reads and validates a configuration file.
func Load(name string) (*Configuration, error) {
	file, err := Path(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := &Configuration{root: filepath.Dir(file)}
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", pathname.Display(file), err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", pathname.Display(file), err)
	}
	return c, nil
}

// Root returns the directory containing the configuration file.
func (c *Configuration) Root() string {
	return c.root
}

func (c *Configuration) validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1")
	}
	if len(c.Projects) == 0 {
		return fmt.Errorf("at least one project is required")
	}
	if c.Defaults.Collision == "" {
		c.Defaults.Collision = "concat"
	}
	if c.Defaults.Write == "" {
		c.Defaults.Write = "most-specific"
	}
	if err := validateOptions(c.Defaults.Collision, c.Defaults.Write); err != nil {
		return err
	}
	for name, project := range c.Projects {
		if _, err := projectPath(c.root, name); err != nil {
			return fmt.Errorf("project %q: %w", name, err)
		}
		if project.Path == "" {
			return fmt.Errorf("project %s: path is required", name)
		}
		project.Path = c.absolute(project.Path)
		c.Projects[name] = project
	}
	seen := make(map[string]bool)
	for i := range c.Overlays {
		o := &c.Overlays[i]
		if o.Name == "" || seen[o.Name] {
			return fmt.Errorf("overlay names must be nonempty and unique: %q", o.Name)
		}
		seen[o.Name] = true
		if o.Source == "" {
			return fmt.Errorf("overlay %s: source is required", o.Name)
		}
		o.Source = c.absolute(o.Source)
		if len(o.Projects) == 0 {
			return fmt.Errorf("overlay %s: projects selectors are required", o.Name)
		}
		for _, selector := range o.Projects {
			if !doublestar.ValidatePattern(selector) {
				return fmt.Errorf("overlay %s: invalid project selector %q", o.Name, selector)
			}
		}
		if o.Glob == "" {
			o.Glob = "**/*"
		}
		if !doublestar.ValidatePattern(o.Glob) || strings.HasPrefix(o.Glob, "/") || strings.Contains(o.Glob, "\\") {
			return fmt.Errorf("overlay %s: invalid relative glob %q", o.Name, o.Glob)
		}
		for _, component := range strings.Split(o.Glob, "/") {
			if component == "." || component == ".." {
				return fmt.Errorf("overlay %s: glob must not contain . or .. components", o.Name)
			}
		}
		if o.Collision == "" {
			o.Collision = c.Defaults.Collision
		}
		if o.Write == "" {
			o.Write = c.Defaults.Write
		}
		if err := validateOptions(o.Collision, o.Write); err != nil {
			return fmt.Errorf("overlay %s: %w", o.Name, err)
		}
	}
	return nil
}

func validateOptions(collision, write string) error {
	if collision != "concat" {
		return fmt.Errorf("unsupported collision policy %q; use concat", collision)
	}
	if write != "most-specific" {
		return fmt.Errorf("unsupported write policy %q; use most-specific", write)
	}
	return nil
}

func (c *Configuration) absolute(name string) string {
	var abs string
	if filepath.IsAbs(name) {
		abs = filepath.Clean(name)
	} else {
		abs = filepath.Join(c.root, name)
	}
	return pathname.Canonical(abs)
}

// Selections returns the selected project, or every project in name order when project is empty.
func (c *Configuration) Selections(project string) ([]Selection, error) {
	if project != "" {
		if _, ok := c.Projects[project]; !ok {
			return nil, fmt.Errorf("project %q is not configured", project)
		}
	}
	names := make([]string, 0, len(c.Projects))
	for name := range c.Projects {
		if project == "" || project == name {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var selections []Selection
	for _, name := range names {
		selection := Selection{Root: c.root, Project: name, Target: c.Projects[name].Path}
		for _, overlay := range c.Overlays {
			for _, selector := range overlay.Projects {
				matched, err := doublestar.Match(selector, name)
				if err != nil {
					return nil, err
				}
				if matched {
					selection.Sources = append(selection.Sources, Source{Name: overlay.Name, Path: overlay.Source, Glob: overlay.Glob})
					break
				}
			}
		}
		selections = append(selections, selection)
	}
	return selections, nil
}

func projectPath(root, project string) (string, error) {
	if project == "." || project == ".." || strings.ContainsAny(project, "/\\") || project == "" {
		return "", fmt.Errorf("project must be a directory name")
	}
	return filepath.Abs(filepath.Join(root, project))
}
