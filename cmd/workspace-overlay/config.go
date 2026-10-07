package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/pelletier/go-toml/v2"
)

const configName = "workspace-overlay.toml"

type overlayOptions struct {
	Collision string `toml:"collision"`
	Write     string `toml:"write"`
}

type projectConfig struct {
	Path string `toml:"path"`
}

type overlayConfig struct {
	Name      string   `toml:"name"`
	Source    string   `toml:"source"`
	Projects  []string `toml:"projects"`
	Glob      string   `toml:"glob"`
	Collision string   `toml:"collision"`
	Write     string   `toml:"write"`
}

type configuration struct {
	Version  int                      `toml:"version"`
	Defaults overlayOptions           `toml:"defaults"`
	Projects map[string]projectConfig `toml:"projects"`
	Overlays []overlayConfig          `toml:"overlays"`
	root     string
}

type overlaySource struct{ name, path, glob string }

func configPath(name string) (string, error) {
	if name != "" {
		return filepath.Abs(name)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		name := filepath.Join(dir, configName)
		if _, err := os.Stat(name); err == nil {
			return name, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%s not found; specify --config", configName)
		}
		dir = parent
	}
}

func loadConfig(name string) (*configuration, error) {
	file, err := configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := &configuration{root: filepath.Dir(file)}
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", displayPath(file), err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", displayPath(file), err)
	}
	return c, nil
}

func (c *configuration) validate() error {
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

func (c *configuration) absolute(name string) string {
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	return filepath.Join(c.root, name)
}

func (c *configuration) selections(project string, replace, worktrees bool) ([]mountSelection, error) {
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
	var selections []mountSelection
	for _, name := range names {
		selection := mountSelection{root: c.root, project: name, target: c.Projects[name].Path, replace: replace, worktrees: worktrees}
		for _, overlay := range c.Overlays {
			for _, selector := range overlay.Projects {
				matched, err := doublestar.Match(selector, name)
				if err != nil {
					return nil, err
				}
				if matched {
					selection.sources = append(selection.sources, overlaySource{name: overlay.Name, path: overlay.Source, glob: overlay.Glob})
					break
				}
			}
		}
		selections = append(selections, selection)
	}
	return selections, nil
}
