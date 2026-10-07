`workspace-overlay` gives developers and coding agents a writable, layered view of project directories. Keep shared instructions, skills, and project-specific files in separate source directories, then expose them together in each project and its Git worktrees. This is useful when you are working on open source or enterprise projects and can't commit your AI files into the project. It especially useful if your workspace consists of multiple repositories and you want to have workspace and per-project AI files.

# What It Does

- **Shared overlays:** Apply ordered overlay directories to any configured projects.
- **Recursive merges:** Merge directories at every depth and concatenate every colliding regular file.
- **Writable files:** Save edits to the most-specific contributing layer while preserving earlier contributions.
- **Live worktrees:** Detect registered Git worktrees and mount the same configured overlay sources into each checkout.
- **Protected sources:** Reject deleting or moving existing overlay files through mounted projects.
- **Temporary Git exclusions:** Hide overlay-only paths from Git while mounted and remove managed exclusions on shutdown.

# How It Works

- Describe projects and ordered overlays in `workspace-overlay.toml`. Paths are relative to that file, so every worktree uses the original sources without `.ai` symlinks.
- Put shared files in a workspace source such as `.ai/workspace/`, and project files in a source such as `.ai/alpha/`. Store skills inside each source's `.agents/skills/` directory.
- Run `workspace-overlay overlay mount`. It serves every configured project and its registered worktrees in the foreground.
- Read and edit files at their normal project paths. For example, `alpha/AGENTS.md` contains the backing project's bytes followed by every matching overlay contribution, in declaration order.
- Use `overlay status` to inspect mounts. Press Ctrl-C in the mounting terminal or run `overlay unmount` to stop them and clean up managed Git exclusions.

# How it Really Works

- The mounted view covers the existing project directory. Ordinary project-file edits persist in that directory; overlay edits persist in their source directories. Unmounting reveals the backing project again.
- File concatenation adds no separators and applies to binary files too. Include any needed newline in the source files. Conflicting file and directory types produce an I/O error.
- A full-document save to a concatenated file must preserve every earlier contribution exactly as its prefix. Only the final contribution is replaced. To edit an earlier contribution, edit its source directly. Append and atomic editor saves are supported; truncating a merged file to zero clears only its final contribution.
- New files go into the backing project when their parent has project backing; otherwise they go into the most-specific overlay directory. A restricted overlay glob must include any new overlay path.
- Overlay files and directories cannot be deleted or moved away through a project mount. Delete or move them in the source directory. Stop the project's overlays before recursively removing one of its worktrees.
- Filesystem events trigger worktree and source reconciliation after a 200 ms debounce. A 30-second fallback scan covers missed events. Checkout locations come from `git worktree list --porcelain -z`; they can be outside the workspace.
- `.git` always comes from the backing project. Overlay-only paths get marked blocks in Git's shared `info/exclude`; existing user entries are preserved. Because worktrees share this file, a rule can also hide a matching untracked backing path in another worktree. Tracked files are unaffected by ignore rules. See [Git's ignore documentation](https://git-scm.com/docs/gitignore).
- Mount ownership and exact exclusion blocks are recorded under `<config-directory>/.tmp/workspace-overlay/`. After forced termination, run `overlay unmount` or `overlay mount --replace` to recover. Incomplete cleanup retains its record; edited managed blocks require manual resolution.
- `overlay status` writes tab-separated target paths and filesystem types to stdout, using `unmounted` for inactive targets. With `--worktrees=false`, it prints filesystem types without paths. Status and unmount also inspect previously recorded mounts, including worktrees excluded from fresh discovery.
- Mount messages and diagnostics go to stderr. Paths beneath your home directory use `~/`. Successful commands and clean shutdown exit with status 0; errors print `ERR:` and exit with status 1.
- Set `AGENT=1`, `true`, or `yes` for compact help. `workspace-overlay skill` prints the embedded operating guide in either mode and works offline.
- `fixture create` creates or reuses an isolated verification workspace with two Git projects, Alpha and Beta, and one linked worktree each. Their shared and project overlays include instructions, skills, and nested colliding files. Repeated creation retains edits and worktree changes. Existing foreign or incomplete directories are refused; failed initialization retains its partial files.

# Prerequisites

- Linux with accessible [`/dev/fuse`](https://docs.kernel.org/filesystems/fuse/fuse.html) for mounting the project view.
- [`fusermount3`](https://github.com/libfuse/libfuse) on `PATH` for unmounting. The CLI invokes it for you.
- [`findmnt`](https://man7.org/linux/man-pages/man8/findmnt.8.html) on `PATH` for mount inspection.
- [`git`](https://git-scm.com/docs/git-worktree) on `PATH` for versioned projects, worktree discovery, and repository exclusions. Unversioned projects mount without Git metadata.
- Existing, readable project and source directories; writable sources for overlay edits.

# Installation

This checkout has no configured release download. Local build and run instructions are in [AGENTS.md](AGENTS.md).

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

Copy [workspace-overlay.example.toml](workspace-overlay.example.toml) into your workspace as `workspace-overlay.toml`, then adjust its paths and selectors. Create the configured source directories before mounting. All relative paths resolve from the configuration file's directory.

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

The runtime requires Linux. Directory moves across layers, hard links to concatenated files, rename flags, special-file creation, and native extended attributes are unsupported. Open merged files retain their original snapshots; reopen them to read updated source contributions.

# Options & Flags

Root options:

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--help` | `-h` | `false` | Print help for any command. |
| `--version` | `-v` | `false` | Print the raw build version; default builds report `0.1.0`. |

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

`workspace-overlay fixture create` (no positional arguments):

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--directory <path>` | | `.tmp/dev-workspace` | Create or reuse the verification workspace, relative to the current directory. |


# License

[MIT](LICENSE), copyright 2026 Alex Gorbatchev.
