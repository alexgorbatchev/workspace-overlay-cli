package overlayfs

import (
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const filesystemName = "workspace-overlay"

// FilesystemType is how a mounted view appears in the mount table.
const FilesystemType = "fuse." + filesystemName

// Mount serves the view over target and returns once the mount is established.
func (v *View) Mount(target string) (*fuse.Server, error) {
	zero := time.Duration(0)
	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			Options: []string{"default_permissions"},
			FsName:  filesystemName,
			Name:    filesystemName,
		},
		EntryTimeout:    &zero,
		AttrTimeout:     &zero,
		NegativeTimeout: &zero,
	}
	return fs.Mount(target, &node{view: v, path: "."}, opts)
}
