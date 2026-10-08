package overlayfs

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hanwen/go-fuse/v2/fuse"
)

var readProcessComm = func(pid uint32) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// isGitCaller reports whether the process making the FUSE filesystem call is Git.
func isGitCaller(ctx context.Context) bool {
	caller, ok := fuse.FromContext(ctx)
	if !ok || caller.Pid == 0 {
		return false
	}
	comm := readProcessComm(caller.Pid)
	return comm == "git" || strings.HasPrefix(comm, "git-")
}
