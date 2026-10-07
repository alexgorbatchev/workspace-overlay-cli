package pathname

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMain(m *testing.M) {
	scratch.Main(m)
}

func TestDisplayPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("get home dir: %v", err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "exact home dir",
			path: home,
			want: "~",
		},
		{
			name: "path inside home",
			path: filepath.Join(home, "projects", "repo"),
			want: "~" + string(filepath.Separator) + filepath.Join("projects", "repo"),
		},
		{
			name: "path outside home",
			path: "/var/log/messages",
			want: "/var/log/messages",
		},
		{
			name: "parent of home",
			path: filepath.Dir(home),
			want: filepath.Dir(home),
		},
		{
			name: "sibling of home",
			path: filepath.Join(filepath.Dir(home), "sibling_dir"),
			want: filepath.Join(filepath.Dir(home), "sibling_dir"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Display(tt.path)
			if got != tt.want {
				t.Errorf("Display(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}

	t.Run("no home dir", func(t *testing.T) {
		t.Setenv("HOME", "")
		got := Display("/some/path")
		if got != "/some/path" {
			t.Errorf("Display with no HOME = %q, want /some/path", got)
		}
	})
}

func TestContains(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		child  string
		want   bool
	}{
		{"same path", "/a/b", "/a/b", true},
		{"child beneath parent", "/a/b", "/a/b/c", true},
		{"parent of child", "/a/b/c", "/a/b", false},
		{"sibling with name prefix", "/a/b", "/a/bc", false},
		{"relative path", "/a/b", "../c", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Contains(tt.parent, tt.child); got != tt.want {
				t.Errorf("Contains(%q, %q) = %v, want %v", tt.parent, tt.child, got, tt.want)
			}
		})
	}
}

func TestCanonical(t *testing.T) {
	t.Run("symlink resolves", func(t *testing.T) {
		tmpdir := t.TempDir()
		target := filepath.Join(tmpdir, "target")
		scratch.Write(t, target, "content")
		link := filepath.Join(tmpdir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if got := Canonical(link); got != target {
			t.Errorf("Canonical(%q) = %q, want %q", link, got, target)
		}
	})
	t.Run("nonexistent path is cleaned", func(t *testing.T) {
		if got := Canonical("/a/../b"); got != "/b" {
			t.Errorf("Canonical(%q) = %q, want %q", "/a/../b", got, "/b")
		}
	})
}
