package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigOrderedSelections(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, configName)
	writeFixture(t, file, `version=1
[projects.frontend]
path="repos/web"
[projects.backend]
path="repos/api"
[[overlays]]
name="common"
source=".ai/common"
projects=["*", "frontend"]
[[overlays]]
name="frontend"
source=".ai/frontend"
projects=["front*"]
glob="**/*.md"
collision="concat"
write="most-specific"
`)
	c, err := loadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	selections, err := c.selections("", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(selections) != 2 || selections[0].project != "backend" || selections[1].project != "frontend" {
		t.Fatalf("ordered projects: %+v", selections)
	}
	if selections[1].target != filepath.Join(root, "repos/web") || !selections[1].replace || !selections[1].worktrees {
		t.Fatalf("resolved selection: %+v", selections[1])
	}
	if len(selections[0].sources) != 1 || len(selections[1].sources) != 2 || selections[1].sources[0].name != "common" || selections[1].sources[1].glob != "**/*.md" {
		t.Fatalf("ordered, deduplicated overlay selection: %+v", selections)
	}
	selected, err := c.selections("frontend", false, false)
	if err != nil || len(selected) != 1 || selected[0].project != "frontend" {
		t.Fatalf("single selection: %+v, %v", selected, err)
	}
	if _, err := c.selections("missing", false, false); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestConfigValidation(t *testing.T) {
	valid := "version=1\n[projects.web]\npath='web'\n"
	overlay := "\n[[overlays]]\nname='common'\nsource='.ai/common'\nprojects=['*']\n"
	cases := []struct{ name, text, want string }{
		{"bad syntax", "version=[", "decode"},
		{"unknown field", valid + "typo=true\n", "decode"},
		{"wrong version", strings.Replace(valid, "version=1", "version=2", 1), "version"},
		{"no projects", "version=1", "project"},
		{"missing project path", "version=1\n[projects.web]", "path"},
		{"invalid project name", strings.Replace(valid, "projects.web", "projects.'a/b'", 1), "directory name"},
		{"bad collision", "version=1\n[defaults]\ncollision='replace'\n[projects.web]\npath='web'", "collision"},
		{"bad write", "version=1\n[defaults]\nwrite='base'\n[projects.web]\npath='web'", "write"},
		{"missing overlay name", valid + strings.Replace(overlay, "name='common'\n", "", 1), "unique"},
		{"duplicate overlay", valid + overlay + overlay, "unique"},
		{"missing source", valid + strings.Replace(overlay, "source='.ai/common'\n", "", 1), "source"},
		{"missing selectors", valid + strings.Replace(overlay, "projects=['*']\n", "", 1), "selectors"},
		{"bad selector", valid + strings.Replace(overlay, "projects=['*']", "projects=['[']", 1), "selector"},
		{"bad glob", valid + overlay + "glob='['\n", "glob"},
		{"absolute glob", valid + overlay + "glob='/*.md'\n", "glob"},
		{"backslash glob", valid + overlay + "glob='a\\b'\n", "glob"},
		{"dot glob", valid + overlay + "glob='./*.md'\n", "components"},
		{"parent glob", valid + overlay + "glob='../*.md'\n", "components"},
		{"overlay policy", valid + overlay + "write='base'\n", "write"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), configName)
			writeFixture(t, file, tt.text)
			if _, err := loadConfig(file); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("loadConfig: %v; want %s", err, tt.want)
			}
		})
	}
	if _, err := loadConfig(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing config accepted")
	}
	t.Run("absolute project", func(t *testing.T) {
		root := t.TempDir()
		file := filepath.Join(root, configName)
		target := filepath.Join(root, "outside-name")
		writeFixture(t, file, "version=1\n[projects.web]\npath='"+target+"'\n")
		c, err := loadConfig(file)
		if err != nil {
			t.Fatal(err)
		}
		selection, err := c.selections("web", false, false)
		if err != nil || selection[0].target != target {
			t.Fatalf("absolute configured target: %+v %v", selection, err)
		}
	})
}

func TestConfigDiscovery(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, configName)
	writeFixture(t, file, "version=1\n[projects.web]\npath='web'\n")
	nested := filepath.Join(root, "child", "nested")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	actual, err := configPath("")
	if err != nil || actual != file {
		t.Fatalf("ancestor discovery: %s, %v", actual, err)
	}
	actual, err = configPath("../../" + configName)
	if err != nil || actual != file {
		t.Fatalf("explicit config: %s, %v", actual, err)
	}
	t.Chdir("/")
	if _, err := configPath(""); err == nil {
		t.Fatal("missing ancestor config accepted")
	}
}
