package session

import (
	"path/filepath"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/config"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestSelectedCarriesMountOptions(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, config.Name)
	scratch.Write(t, file, `version=1
[projects.alpha]
path="alpha"
[projects.beta]
path="beta"
[[overlays]]
name="common"
source=".ai/common"
projects=["*"]
`)
	cfg, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}

	mounts, err := Selections(cfg, "", true, true)
	if err != nil {
		t.Fatalf("selected with all projects failed: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}

	configSelections, err := cfg.Selections("")
	if err != nil {
		t.Fatalf("cfg.Selections failed: %v", err)
	}
	for i, mount := range mounts {
		if mount.Root != configSelections[i].Root {
			t.Errorf("mount %d root: got %q, want %q", i, mount.Root, configSelections[i].Root)
		}
		if mount.Project != configSelections[i].Project {
			t.Errorf("mount %d project: got %q, want %q", i, mount.Project, configSelections[i].Project)
		}
		if mount.Target != configSelections[i].Target {
			t.Errorf("mount %d target: got %q, want %q", i, mount.Target, configSelections[i].Target)
		}
		if !mount.Replace {
			t.Errorf("mount %d replace: got false, want true", i)
		}
		if !mount.Worktrees {
			t.Errorf("mount %d worktrees: got false, want true", i)
		}
		if len(mount.Sources) != len(configSelections[i].Sources) {
			t.Errorf("mount %d sources count: got %d, want %d", i, len(mount.Sources), len(configSelections[i].Sources))
		}
		for j, src := range mount.Sources {
			if src.Name != configSelections[i].Sources[j].Name || src.Path != configSelections[i].Sources[j].Path || src.Glob != configSelections[i].Sources[j].Glob {
				t.Errorf("mount %d source %d mismatch: got %+v, want %+v", i, j, src, configSelections[i].Sources[j])
			}
		}
	}

	mounts, err = Selections(cfg, "beta", false, false)
	if err != nil {
		t.Fatalf("selected with single project failed: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}
	if mounts[0].Project != "beta" || mounts[0].Replace || mounts[0].Worktrees {
		t.Fatalf("single selection mismatch: %+v", mounts[0])
	}

	mounts, err = Selections(cfg, "missing", false, false)
	if err == nil {
		t.Fatalf("expected error for missing project, got nil")
	}
}
