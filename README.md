`workspace-overlay` gives developers and coding agents a writable, layered view of project directories. Keep shared instructions, skills, and project-specific files in separate source directories, then expose them together in each project and its Git worktrees. This is useful when you are working on open source or enterprise projects and can't commit your AI files into the project. It especially useful if your workspace consists of multiple repositories and you want to have workspace and per-project AI files.

# What It Does

- **Shared overlays:** Apply ordered overlay directories to any configured projects.
- **Recursive merges:** Merge directories at every depth. Text files that collide are joined end-to-end; binary collisions show an explanation instead of content.
- **Writable files:** Save edits to the most-specific contributing layer while preserving earlier contributions.
- **Live worktrees:** Detect registered Git worktrees and mount the same configured overlay sources into each checkout.
- **Protected sources:** Reject deleting or moving existing overlay files through mounted projects; entries created through a mount stay removable until it stops.
- **Temporary Git exclusions:** Hide overlay-only paths from Git while mounted and remove managed exclusions on shutdown.

# How It Works

- Describe projects and ordered overlays in `workspace-overlay.toml`. Paths are relative to that file, so every worktree uses the original sources without `.ai` symlinks.
- Put shared files in a workspace source such as `.ai/workspace/`, and project files in a source such as `.ai/alpha/`. Store skills inside each source's `.agents/skills/` directory.
- Run `workspace-overlay overlay mount`. It serves every configured project and its registered worktrees in the foreground.
- Read and edit files at their normal project paths. For example, `alpha/AGENTS.md` contains the backing project's bytes followed by every matching overlay contribution, in declaration order.
- Use `overlay status` to inspect mounts. Press Ctrl-C in the mounting terminal or run `overlay unmount` to stop them and clean up managed Git exclusions.

# How it Really Works

- The mounted view covers the existing project directory. Ordinary project-file edits persist in that directory; overlay edits persist in their source directories. Unmounting reveals the backing project again.
- Text files that collide are joined end to end with no separators, so include any needed newline in the source files. Binary files are never joined: when a path has copies in more than one layer and at least one is binary, reading that path returns an explanation that lists every copy and how to resolve the collision, the path rejects writes, and each open logs the collision to stderr. A copy counts as binary when its first 512 bytes do not look like text.
- A file that comes from a single layer is served unchanged and behaves like an ordinary file, including shared memory mappings; free-space queries on a mounted project report the project's own filesystem. A joined file is held in memory while open and is limited to 64 MiB: opening a larger one, or writing or truncating one past that size, fails with "file too large".
- A file that is a directory in one layer and a regular file in another returns an I/O error when accessed; the containing directory remains listable.
- A full-document save to a merged file must preserve every earlier contribution exactly as its prefix. Only the final contribution is replaced. To edit an earlier contribution, edit its source directly. Append and atomic editor saves are supported; truncating a merged file to zero clears only its final contribution.
- New files go into the backing project when their parent has project backing; otherwise they go into the most-specific overlay directory. A restricted overlay glob must include any new overlay path.
- Existing overlay files and directories cannot be deleted or moved away through a project mount. Delete or move them in the source directory. Stop the project's overlays before recursively removing one of its worktrees.
- Entries created through a mount inside an overlay directory, such as an editor's swap file or a file written under a temporary name, can be renamed to a free name and removed through that mount until it stops. A file renamed over an existing overlay document replaces that document's final contribution (an atomic save), and the document stays protected.
- Filesystem events trigger worktree and source reconciliation after a 200 ms debounce. A 30-second fallback scan covers missed events. Checkout locations come from `git worktree list --porcelain -z`; they can be outside the workspace.
- A linked worktree that cannot be mounted safely (it lies inside another mount target, it contains an overlay source, or its path is already a mount of another filesystem) is skipped with a `Skipping worktree` message on stderr while the project and every other project keep running. It is tried again after Git unregisters and registers it again. Overlapping projects are rejected before anything is mounted.
- `.git` always comes from the backing project. Overlay-only paths get marked blocks in Git's shared `info/exclude`; existing user entries are preserved. Because worktrees share this file, a rule can also hide a matching untracked backing path in another worktree. Tracked files are unaffected by ignore rules. See [Git's ignore documentation](https://git-scm.com/docs/gitignore).
- The configuration file's directory and the project and source paths are resolved through symbolic links, so a workspace reached through a symlink works, and status and mount messages show resolved paths. A configuration file that is itself a symlink resolves its relative paths from the directory the link is in, so a workspace can link to a file kept elsewhere.
- Mount ownership and exact exclusion blocks are recorded under `$XDG_STATE_HOME/workspace-overlay/` (`~/.local/state/workspace-overlay/` when `XDG_STATE_HOME` is unset, empty, or relative), one set of files per workspace and project, removed when the overlay stops cleanly. `overlay mount --replace` first stops the selected projects' existing overlays and then validates and mounts. After forced termination, both `overlay unmount` and `overlay mount --replace` recover. Incomplete cleanup retains its record; edited managed blocks require manual resolution.
- `overlay status` always prints one line per target: the target path, a tab, and the filesystem type (`unmounted` when inactive). `--worktrees=false` only limits discovery to primary projects; recorded mounts are still listed. Status and unmount also inspect previously recorded mounts, including worktrees excluded from fresh discovery.
- Mount messages and diagnostics go to stderr. Paths beneath your home directory use `~/`. Successful commands and clean shutdown exit with status 0. Failed operations print only `ERR: <message>` on stderr and exit with status 1; mistyped commands additionally print the command's usage on stderr before the error line.
- Set `AGENT=1`, `true`, or `yes` for compact help. `workspace-overlay skill` prints the embedded operating guide in either mode and works offline.
- `fixture create` creates or reuses an isolated verification workspace with two Git projects, Alpha and Beta, and one linked worktree each. Their shared and project overlays include instructions, skills, and nested colliding files. Repeated creation retains edits and worktree changes. Existing foreign or incomplete directories are refused; failed initialization retains its partial files.

# Installation

Download the prebuilt Linux binary for your architecture from the [latest release](https://github.com/alexgorbatchev/workspace-overlay-cli/releases/latest).

```sh
# Linux (x86-64)
curl -sSL https://github.com/alexgorbatchev/workspace-overlay-cli/releases/download/v0.0.2/workspace-overlay_0.0.2_linux_amd64.tar.gz | tar -xz -C ~/.local/bin workspace-overlay
```

# Setup

- Linux with accessible [`/dev/fuse`](https://docs.kernel.org/filesystems/fuse/fuse.html) for mounting the project view.
- [`fusermount3`](https://github.com/libfuse/libfuse) on `PATH` for mounting and unmounting. The CLI invokes it for you.
- [`findmnt`](https://man7.org/linux/man-pages/man8/findmnt.8.html) on `PATH` for mount inspection.
- [`git`](https://git-scm.com/docs/git-worktree) on `PATH` for versioned projects, worktree discovery, and repository exclusions. Unversioned projects mount without Git metadata.
- Existing, readable project and source directories; writable sources for overlay edits.

# Quick Start

Run from the directory containing `workspace-overlay.toml`, with the executable on `PATH`:

```sh
# Check the selected project before mounting.
workspace-overlay overlay status --project alpha --worktrees=false
```

```sh
# Serve all configured projects and worktrees in the foreground.
workspace-overlay overlay mount
```

In another terminal in the same workspace:

```sh
# Inspect all registered and recorded mounts.
workspace-overlay overlay status

# Stop all configured projects and their recorded mounts.
workspace-overlay overlay unmount
```

For an explicit configuration path or a narrower mount:

```sh
workspace-overlay overlay mount --config workspace-overlay.toml --project alpha
```

# Configuration

Copy [workspace-overlay.example.toml](workspace-overlay.example.toml) into your workspace as `workspace-overlay.toml`, then adjust its paths and selectors. Create the configured source directories before mounting. All relative paths resolve from the configuration file's directory; for a symlinked configuration file, that is the directory holding the link.

Example `workspace-overlay.toml`:

```toml
version = 1

[defaults]
collision = "concat"
write = "most-specific"

[projects.alpha]
path = "alpha"

[projects.beta]
path = "beta"

[[overlays]]
name = "workspace"
source = ".ai/workspace"
projects = ["*"]
glob = "**/*"

[[overlays]]
name = "beta"
source = ".ai/beta"
projects = ["beta"]
glob = "**/*"

[[overlays]]
name = "alpha"
source = ".ai/alpha"
projects = ["alpha"]
glob = "**/*"
```

Each project requires a `path`. Each overlay requires a unique `name`, a `source`, and `projects` glob selectors that match configured project names. Overlay declaration order determines concatenation order after the backing project.

`glob` defaults to `**/*`, includes dotfolders, and selects source paths at any depth. Use relative forward-slash patterns; `.` and `..` components are rejected. For example, `glob = ".agents/skills/**"` selects only skills and their ancestor directories.

Per-overlay `collision` and `write` fields override `[defaults]`. The supported values are `concat` and `most-specific`, respectively. Unknown TOML fields, invalid patterns, overlapping mount targets, and sources inside mount targets are rejected.

# Limitations

The runtime requires Linux. Directory moves across layers, hard links to merged files, rename flags, special-file creation, and native extended attributes are unsupported. Open merged files retain their original snapshots; reopen them to read updated source contributions. Merged files are held in memory and limited to 64 MiB total.

When the calling process is Git, `workspace-overlay` serves the base project layer directly, so Git status stays clean and branch checkouts succeed. Overlay contributions can be delineated with start and end markers with relative paths from the project directory.

# Options & Flags

Root options:

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--help` | `-h` | `false` | Print help for any command. |
| `--version` | `-v` | `false` | Print the raw version: the release version for a released binary, `dev` for any other build. |

`workspace-overlay overlay` and its commands:

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--config <path>` | | Empty | Discover the nearest `workspace-overlay.toml` in the current or parent directories. |
| `--project <name>` | | Empty | Select all configured projects; supply a configured name to narrow the selection. |

`workspace-overlay overlay mount`:

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--worktrees` | | `true` | Discover and monitor Git worktrees; use `--worktrees=false` to mount primary projects only. |
| `--replace` | | `false` | Stop existing overlays and recover stale registrations before mounting. Foreign filesystems are refused. |

`workspace-overlay overlay status` and `workspace-overlay overlay unmount`:

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--worktrees` | | `true` | Include Git worktree discovery. Recorded mounts remain included when false; unmount stops the selected owner and its mounts. |

These overlay commands accept no positional arguments. `workspace-overlay help [command]` prints command help. `workspace-overlay skill` accepts no arguments or command-specific flags.


# License

[MIT](LICENSE), copyright 2026 Alex Gorbatchev.
