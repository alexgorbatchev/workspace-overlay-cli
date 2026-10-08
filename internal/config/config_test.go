package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestConfigOrderedSelections(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, Name)
	scratch.Write(t, file, `version=1
[projects.frontend]
path="repos/web"
[projects.backend]
path="repos/api"
[[markers]]
glob="*.md"
start="<!-- BEGIN {name} ({path}) -->"
end="<!-- END {name} -->"
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
	c, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	selections, err := c.Selections("")
	if err != nil {
		t.Fatal(err)
	}
	if len(selections) != 2 || selections[0].Project != "backend" || selections[1].Project != "frontend" {
		t.Fatalf("ordered projects: %+v", selections)
	}
	if selections[1].Target != filepath.Join(root, "repos/web") {
		t.Fatalf("resolved selection: %+v", selections[1])
	}
	if len(selections[0].Sources) != 1 || len(selections[1].Sources) != 2 || selections[1].Sources[0].Name != "common" || selections[1].Sources[1].Glob != "**/*.md" {
		t.Fatalf("ordered, deduplicated overlay selection: %+v", selections)
	}
	if len(selections[0].Markers) != 1 || selections[0].Markers[0].Glob != "*.md" {
		t.Fatalf("markers not parsed: %+v", selections[0].Markers)
	}
	selected, err := c.Selections("frontend")
	if err != nil || len(selected) != 1 || selected[0].Project != "frontend" {
		t.Fatalf("single selection: %+v, %v", selected, err)
	}
	if _, err := c.Selections("missing"); err == nil {
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
		{"missing marker glob", valid + "\n[[markers]]\nstart='a'\nend='b'\n", "requires glob"},
		{"missing marker start", valid + "\n[[markers]]\nglob='*.md'\nend='b'\n", "requires start"},
		{"missing marker end", valid + "\n[[markers]]\nglob='*.md'\nstart='a'\n", "requires start and end"},
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
			file := filepath.Join(t.TempDir(), Name)
			scratch.Write(t, file, tt.text)
			if _, err := Load(file); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load: %v; want %s", err, tt.want)
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing config accepted")
	}
	t.Run("absolute project", func(t *testing.T) {
		root := t.TempDir()
		file := filepath.Join(root, Name)
		target := filepath.Join(root, "outside-name")
		scratch.Write(t, file, "version=1\n[projects.web]\npath='"+target+"'\n")
		c, err := Load(file)
		if err != nil {
			t.Fatal(err)
		}
		selection, err := c.Selections("web")
		if err != nil || selection[0].Target != target {
			t.Fatalf("absolute configured target: %+v %v", selection, err)
		}
	})
}

func TestConfigDiscovery(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, Name)
	scratch.Write(t, file, "version=1\n[projects.web]\npath='web'\n")
	nested := filepath.Join(root, "child", "nested")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	actual, err := Path("")
	if err != nil || actual != file {
		t.Fatalf("ancestor discovery: %s, %v", actual, err)
	}
	actual, err = Path("../../" + Name)
	if err != nil || actual != file {
		t.Fatalf("explicit config: %s, %v", actual, err)
	}
	t.Chdir("/")
	if _, err := Path(""); err == nil {
		t.Fatal("missing ancestor config accepted")
	}
}

func TestLostWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Path(""); err == nil {
		t.Fatal("discovery accepted removed working directory")
	}
	if _, err := Load(""); err == nil {
		t.Fatal("load accepted removed working directory")
	}
	if _, err := projectPath(".", "project"); err == nil {
		t.Fatal("relative project accepted removed working directory")
	}
}

func TestProjectPath(t *testing.T) {
	tests := []struct {
		name      string
		root      string
		project   string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "valid relative",
			root:    "..",
			project: "alpha",
			wantErr: false,
		},
		{
			name:    "valid current dir",
			root:    ".",
			project: "my-app",
			wantErr: false,
		},
		{
			name:      "empty project",
			root:      ".",
			project:   "",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
		{
			name:      "dot project",
			root:      ".",
			project:   ".",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
		{
			name:      "dot dot project",
			root:      ".",
			project:   "..",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
		{
			name:      "slash in project",
			root:      ".",
			project:   "nested/dir",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
		{
			name:      "backslash in project",
			root:      ".",
			project:   "nested\\dir",
			wantErr:   true,
			errSubstr: "project must be a directory name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := projectPath(tt.root, tt.project)
			if (err != nil) != tt.wantErr {
				t.Fatalf("projectPath(%q, %q) error = %v, wantErr %v", tt.root, tt.project, err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.errSubstr)
				}
			} else {
				if !filepath.IsAbs(got) {
					t.Errorf("expected absolute path, got %q", got)
				}
				expected := filepath.Clean(filepath.Join(tt.root, tt.project))
				expectedAbs, err := filepath.Abs(expected)
				if err != nil {
					t.Fatalf("filepath.Abs(%q): %v", expected, err)
				}
				if got != expectedAbs {
					t.Errorf("got %q, want %q", got, expectedAbs)
				}
			}
		})
	}
}

func TestSymlinkedConfigResolvesToRealTarget(t *testing.T) {
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(resolved, "real")
	scratch.GitRepo(t, filepath.Join(workspace, "project"))
	scratch.Write(t, filepath.Join(workspace, Name), "version=1\n[projects.project]\npath='project'\n")
	alias := filepath.Join(resolved, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(alias, Name))
	if err != nil {
		t.Fatal(err)
	}
	selections, err := cfg.Selections("")
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(workspace, "project")
	if selections[0].Target != expected {
		t.Errorf("target: got %q, want %q (should not contain 'alias')", selections[0].Target, expected)
	}
	if selections[0].Root != workspace {
		t.Errorf("root: got %q, want %q", selections[0].Root, workspace)
	}
}

// A configuration file that is itself a symbolic link belongs to the directory the
// link is in, so a workspace can keep the file elsewhere and still name its projects
// and sources from its own root.
func TestLinkedConfigResolvesFromTheLinkDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	project := filepath.Join(workspace, "project")
	scratch.GitRepo(t, project)
	source := scratch.Mkdir(t, filepath.Join(workspace, "layers", "shared"))
	stored := scratch.Write(t, filepath.Join(root, "stored", Name), "version=1\n[projects.project]\npath='project'\n[[overlays]]\nname='shared'\nsource='layers/shared'\nprojects=['*']\n")
	link := filepath.Join(workspace, Name)
	if err := os.Symlink(stored, link); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}

	tests := []struct{ name, dir, config string }{
		{"named link", root, link},
		{"link named through a symlinked workspace", root, filepath.Join(alias, Name)},
		{"link discovered from a project", project, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.dir)
			file, err := Path(tt.config)
			if err != nil || file != link {
				t.Fatalf("Path(%q) = %q, %v; want the link %q", tt.config, file, err, link)
			}
			cfg, err := Load(tt.config)
			if err != nil {
				t.Fatal(err)
			}
			selections, err := cfg.Selections("")
			if err != nil {
				t.Fatal(err)
			}
			if got := selections[0].Root; got != workspace {
				t.Errorf("root = %q, want the directory holding the link %q", got, workspace)
			}
			if got := selections[0].Target; got != project {
				t.Errorf("target = %q, want %q", got, project)
			}
			if got := selections[0].Sources[0].Path; got != source {
				t.Errorf("source = %q, want %q", got, source)
			}
		})
	}
}

// A project or overlay source that is itself a symbolic link is addressed by
// its real location, the directory the mount and the watcher act on.
func TestSymlinkedProjectAndSourceResolveToRealPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "real-project")
	scratch.GitRepo(t, project)
	source := scratch.Mkdir(t, filepath.Join(root, "real-source"))
	for link, target := range map[string]string{"project": project, "source": source} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	scratch.Write(t, filepath.Join(root, Name), "version=1\n[projects.project]\npath='project'\n[[overlays]]\nname='shared'\nsource='source'\nprojects=['*']\n")

	cfg, err := Load(filepath.Join(root, Name))
	if err != nil {
		t.Fatal(err)
	}
	selections, err := cfg.Selections("")
	if err != nil {
		t.Fatal(err)
	}
	if got := selections[0].Target; got != project {
		t.Errorf("target = %q, want the real directory %q", got, project)
	}
	if got := selections[0].Sources[0].Path; got != source {
		t.Errorf("source = %q, want the real directory %q", got, source)
	}
}

func TestMain(m *testing.M) {
	scratch.Main(m)
}
