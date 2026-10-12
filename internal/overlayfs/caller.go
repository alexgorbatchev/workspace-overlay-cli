package overlayfs

import (
	"context"
	"strconv"
	"strings"

	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

var readProcessComm = func(pid uint32) string {
	name := "/proc/" + strconv.FormatUint(uint64(pid), 10) + "/comm"
	var fd int
	var err error
	for {
		fd, err = unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return ""
	}
	defer func() {
		_ = unix.Close(fd) // Read-only descriptor; preserve the successful read on a close error.
	}()
	// Linux comm is at most 15 bytes plus a newline. Read it afresh without
	// stat, buffer growth, or registering this synchronous proc file with epoll.
	var data [16]byte
	n := 0
	for n < len(data) {
		count, err := unix.Read(fd, data[n:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return ""
		}
		if count == 0 {
			break
		}
		n += count
	}
	return strings.TrimSpace(string(data[:n]))
}

func callerDependent(parts []contribution) bool {
	return len(parts) != 1 || parts[0].index != 0
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
