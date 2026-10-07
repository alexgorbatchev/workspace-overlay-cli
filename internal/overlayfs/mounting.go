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
//
// It leaves out the access to the new mount that fs.Mount makes through
// Server.WaitMount. A process killed in the middle of a file operation on a
// mount it serves never exits, because the request waits for an answer only
// that process could give, so the serving process must not touch its mounts.
// A caller that does read its own mount, as tests do, calls WaitMount itself.
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
	// NewServer returns once the mount exists and its handshake with the
	// kernel is done.
	server, err := fuse.NewServer(fs.NewNodeFS(&node{view: v, path: "."}, opts), target, &opts.MountOptions)
	if err != nil {
		return nil, err
	}
	go server.Serve()
	return server, nil
}
