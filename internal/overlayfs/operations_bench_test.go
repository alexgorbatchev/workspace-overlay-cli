package overlayfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

// BenchmarkOperations compares the same application calls on native files and
// a real mount. Setup and teardown are excluded; each sub-benchmark has its own
// fixture so mutations cannot change another operation's workload.
func BenchmarkOperations(b *testing.B) {
	for _, backend := range []string{"native", "fuse", "fuse-restricted"} {
		for _, operation := range []string{
			"stat_project", "stat_merged", "stat_marked", "negative_lookup", "readdir", "readdir_stat",
			"read_project", "read_merged", "read_marked", "read_large",
			"write_project", "write_merged", "create_remove", "mkdir_remove",
			"rename", "chmod", "truncate", "readlink", "symlink_remove", "link_remove", "statfs",
			"read_handle", "write_handle", "stat_handle", "sync_handle", "access", "chtimes", "chown",
		} {
			b.Run(backend+"/"+operation, func(b *testing.B) {
				dir := benchmarkWorkspace(b, backend)
				project := filepath.Join(dir, "project.txt")
				merged := filepath.Join(dir, "merged.txt")
				marked := filepath.Join(dir, "marked.md")
				other := filepath.Join(dir, "other")
				link := filepath.Join(dir, "link")
				data := []byte(strings.Repeat("a", 4096))
				mergedData, err := os.ReadFile(merged)
				if err != nil {
					b.Fatal(err)
				}
				var op func() error
				switch operation {
				case "stat_project", "stat_merged", "stat_marked":
					name := map[string]string{"stat_project": project, "stat_merged": merged, "stat_marked": marked}[operation]
					op = func() error { _, err := os.Stat(name); return err }
				case "negative_lookup":
					op = func() error {
						_, err := os.Stat(other)
						if os.IsNotExist(err) {
							return nil
						}
						return fmt.Errorf("missing lookup: %v", err)
					}
				case "readdir":
					name := filepath.Join(dir, "directory")
					op = func() error { _, err := os.ReadDir(name); return err }
				case "readdir_stat":
					name := filepath.Join(dir, "directory")
					op = func() error {
						entries, err := os.ReadDir(name)
						if err != nil {
							return err
						}
						for _, entry := range entries {
							if _, err := entry.Info(); err != nil {
								return err
							}
						}
						return nil
					}
				case "read_project", "read_merged", "read_marked", "read_large":
					name := map[string]string{"read_project": project, "read_merged": merged, "read_marked": marked, "read_large": filepath.Join(dir, "large.txt")}[operation]
					op = func() error { _, err := os.ReadFile(name); return err }
				case "write_project":
					op = func() error { return os.WriteFile(project, data, 0644) }
				case "write_merged":
					op = func() error { return os.WriteFile(merged, mergedData, 0644) }
				case "create_remove":
					op = func() error {
						if err := os.WriteFile(other, data, 0644); err != nil {
							return err
						}
						return os.Remove(other)
					}
				case "mkdir_remove":
					op = func() error {
						if err := os.Mkdir(other, 0755); err != nil {
							return err
						}
						return os.Remove(other)
					}
				case "rename":
					op = func() error {
						if err := os.Rename(project, other); err != nil {
							return err
						}
						return os.Rename(other, project)
					}
				case "chmod":
					op = func() error { return os.Chmod(project, 0644) }
				case "truncate":
					op = func() error { return os.Truncate(project, 4096) }
				case "readlink":
					op = func() error { _, err := os.Readlink(link); return err }
				case "symlink_remove", "link_remove":
					op = func() error {
						var err error
						if operation == "symlink_remove" {
							err = os.Symlink("project.txt", other)
						} else {
							err = os.Link(project, other)
						}
						if err != nil {
							return err
						}
						return os.Remove(other)
					}
				case "statfs":
					var stat syscall.Statfs_t
					op = func() error { return syscall.Statfs(dir, &stat) }
				case "read_handle", "write_handle", "stat_handle", "sync_handle":
					file, err := os.OpenFile(project, os.O_RDWR, 0)
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() {
						if err := file.Close(); err != nil {
							b.Error(err)
						}
					})
					switch operation {
					case "read_handle":
						op = func() error { _, err := file.ReadAt(data, 0); return err }
					case "write_handle":
						op = func() error { _, err := file.WriteAt(data, 0); return err }
					case "stat_handle":
						op = func() error { _, err := file.Stat(); return err }
					case "sync_handle":
						op = file.Sync
					}
				case "access":
					op = func() error { return syscall.Access(project, 4) }
				case "chtimes":
					now := time.Now()
					op = func() error { return os.Chtimes(project, now, now) }
				case "chown":
					op = func() error { return os.Chown(project, os.Getuid(), os.Getgid()) }
				}
				b.ResetTimer()
				for b.Loop() {
					if err := op(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func benchmarkWorkspace(b *testing.B, backend string) string {
	b.Helper()
	dir := b.TempDir()
	project := scratch.Mkdir(b, filepath.Join(dir, "project"))
	source := scratch.Mkdir(b, filepath.Join(dir, "source"))
	data := strings.Repeat("a", 4096)
	for _, name := range []string{"project.txt", "merged.txt", "marked.md"} {
		scratch.Write(b, filepath.Join(project, name), data)
	}
	scratch.Write(b, filepath.Join(project, "large.txt"), strings.Repeat("a", 1<<20))
	for i := range 128 {
		scratch.Write(b, filepath.Join(project, "directory", fmt.Sprintf("file-%03d.txt", i)), data)
	}
	if err := os.Symlink("project.txt", filepath.Join(project, "link")); err != nil {
		b.Fatal(err)
	}
	for _, name := range []string{"merged.txt", "marked.md"} {
		scratch.Write(b, filepath.Join(source, name), data)
	}
	v := &View{}
	for i, name := range []string{project, source} {
		root, err := os.OpenRoot(name)
		if err != nil {
			b.Fatal(err)
		}
		if i == 0 {
			v.AddProject(root)
		} else {
			glob := "**/*"
			if backend == "fuse-restricted" {
				glob = "{merged.txt,marked.md}"
			}
			v.AddOverlay(root, "source", glob)
		}
	}
	v.SetMarkers(DefaultMarkerRules())
	if err := v.RefreshPaths(); err != nil {
		b.Fatal(err)
	}
	if backend == "native" {
		child := &node{view: v, path: "marked.md"}
		parts, err := v.resolve("marked.md")
		if err != nil {
			b.Fatal(err)
		}
		marked, err := child.renderMarkedData(parts, v.findMarkerRule("marked.md"))
		if err != nil {
			b.Fatal(err)
		}
		scratch.Write(b, filepath.Join(project, "merged.txt"), data+data)
		scratch.Write(b, filepath.Join(project, "marked.md"), string(marked))
		v.Close()
		return project
	}
	server, err := v.Mount(project)
	if err != nil {
		v.Close()
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			b.Error(err)
		}
		v.Close()
	})
	if err := server.WaitMount(); err != nil {
		b.Fatal(err)
	}
	return project
}
