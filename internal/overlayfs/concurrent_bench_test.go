package overlayfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func BenchmarkConcurrentMetadata(b *testing.B) {
	for _, count := range []int{1, 128} {
		b.Run(fmt.Sprintf("files-%d", count), func(b *testing.B) {
			var v View
			names := make([]string, count)
			for i := range names {
				names[i] = fmt.Sprintf("file-%d.txt", i)
			}
			for i := range 2 {
				dir := b.TempDir()
				for _, name := range names {
					scratch.Write(b, filepath.Join(dir, name), "contents")
				}
				r, err := os.OpenRoot(dir)
				if err != nil {
					b.Fatal(err)
				}
				if i == 0 {
					v.AddProject(r)
				} else {
					v.AddOverlay(r, "source", "**/*")
				}
			}
			b.Cleanup(v.Close)
			if err := v.RefreshPaths(); err != nil {
				b.Fatal(err)
			}
			root := &node{view: &v, path: "."}
			fs.NewNodeFS(root, nil)
			ctx := fuse.NewContext(context.Background(), &fuse.Caller{Pid: uint32(os.Getpid())})
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					var out fuse.EntryOut
					if _, errno := root.Lookup(ctx, names[i%count], &out); errno != 0 {
						b.Error(errno)
						return
					}
					i++
				}
			})
		})
	}
}
