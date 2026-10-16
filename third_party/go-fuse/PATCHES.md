---
created_on: 2026-10-12 22:42
last_modified: 2026-10-15 15:37
status: current
---

# go-fuse source and local patches

This directory contains the complete Go module source for
[`github.com/hanwen/go-fuse/v2` v2.11.0](https://github.com/hanwen/go-fuse/tree/423b377e1452ab7b3522229185a3047f72e3f966),
commit `423b377e1452ab7b3522229185a3047f72e3f966`. The root module's `replace`
directive selects this source. The upstream license is retained in `LICENSE`.

The upstream module archive checksum is
`h1:CGVkJh9gRz0pTRMADNcqdFl3ec/5QbE/Vx1Gl7ESozM=`; its `go.mod` checksum is
`h1:aU7NkGYZUmuJrZapoI3mEcNve7PZTySUOLBuch/vR6U=`.

## Path attributes and open descriptors

`fs/bridge.go`, `rawBridge.GetAttr`, delegates handle-free attribute requests
to a linked node's `NodeGetattrer` instead of borrowing an arbitrary open
descriptor. Borrowing a handle crosses workspace-overlay's caller-dependent
views: a Git path stat can receive an editor's merged size, or an editor can
receive Git's project-only size.

Explicit descriptor requests retain their handle. Unlinked nodes retain the
upstream handle fallback because the kernel can omit `FUSE_GETATTR_FH` for
descriptor stats. Nodes without `NodeGetattrer` also retain that fallback.

The exact source change is recorded in `path-getattr.patch`. Project
regressions cover alternating Git/editor callers with handles kept open,
staged-write sizes, explicit descriptor requests, and mounted descriptor
stats after unlink:

```sh
GOMAXPROCS=1 go test ./internal/overlayfs -count=10 -run 'TestPathStatWithAnotherCallersOpenHandle|TestMetadataAlternatingKernelCallers|TestMetadataSourceTransitionKeepsAttributesUncached|TestMetadataDescriptorStatAfterUnlink'
```

Run this command from the workspace-overlay repository root.

## Passthrough handle ownership

`fs/bridge.go` checks each handle's `PassthroughFd` decision even when the inode
already has a registered backing ID. Each file entry records whether its open
acquired a backing reference; releasing an ordinary handle cannot decrement
another handle's reference. CREATE acquires backing references under the same
bridge lock used by OPEN and RELEASE.

The source change is recorded in `passthrough-lifetime.patch`, applied after
`path-getattr.patch`. `fs/passthrough_lifetime_test.go` exercises a declined
handle alongside an existing backing registration and mixed-handle release.
Run these tests separately from the root module:

```sh
mkdir -p .tmp
cd third_party/go-fuse
TMPDIR="$PWD/../../.tmp" go test -race ./fs -run 'TestPassthrough|TestReleaseWithoutPassthrough' -count=1 -v
```

The upstream mounted passthrough test requires kernel support and
`CAP_SYS_ADMIN`; it skips when these are unavailable. The two ownership
regressions run without those capabilities. Kernel negotiation and failed
registration continue to fall back to ordinary FUSE I/O.
