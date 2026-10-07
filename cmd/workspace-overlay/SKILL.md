---
name: workspace-overlay
description: Use when mounting, inspecting, or stopping workspace-overlay and editing its merged project view.
author: alexgorbatchev
metadata:
  created_on: 2026-10-06 16:00
  last_modified: 2026-10-06 21:45
  status: current
---

Run `workspace-overlay overlay mount` in a directory with `workspace-overlay.toml` in its ancestry, or supply `--config path/to/workspace-overlay.toml`. Require Linux, accessible `/dev/fuse`, Git for versioned projects, `findmnt`, and `fusermount3`.

- `overlay mount`: Serve every configured project and its Git worktrees in the foreground. Accept no positional arguments. SIGINT/SIGTERM stops all mounts. Mount failures cancel the group. External unmount of a primary project stops that project's mounts; external unmount of a linked worktree suppresses it until it is unregistered and registered again.
- `overlay unmount`: Stop selected owner processes and clean their mounts and managed Git exclusions. Recover recorded mounts from terminated owners. Succeed for unmounted targets; refuse filesystems other than `fuse.workspace-overlay`.
- `overlay status`: Print one tab-separated target path and filesystem type per target, including recorded mounts that Git no longer lists. Print `unmounted` for unmounted targets. With `--worktrees=false`, print only the type for each discovered or recorded target. Return exit 0 for unmounted targets.
- `--config` (string, default empty): Apply to all overlay commands. Empty discovers the nearest `workspace-overlay.toml` in the current or parent directories. Resolve configured project and source paths relative to the file's directory.
- `--project` (string, default empty): Apply to all overlay commands. Empty selects all configured projects in sorted name order. A supplied name must exist in `[projects]`.
- `--worktrees` (bool, default true): Apply to all overlay commands. Include registered Git worktrees; mount monitors changes. Set `--worktrees=false` to mount primary projects only or omit fresh worktree discovery for status and unmount. Status and unmount still include recorded mounts; unmount stops the selected owner and all its mounts. Skip bare, prunable, and initializing worktree records. Projects without their own `.git` mount alone.
- `--replace` (bool, default false): Apply to mount only. Stop selected owners and recover stale registrations before mounting again. Refuse foreign filesystem replacement.
- `skill`: Print this embedded guide verbatim, offline, without positional arguments or command-specific flags.
- `help [command]`: Print help for a command path. `--help` / `-h` (bool, default false) prints help on any command. Shell completion generation is disabled.
- `--version` / `-v` (bool, default false): Apply to the root. Print the raw build version followed by a newline; development builds report `0.1.0-dev`.

Set `AGENT=1`, `true`, or `yes` for compact help with a skill-reading alert; case and surrounding whitespace are ignored. Send progress and diagnostics to stderr. Abbreviate home paths with `~/`, and the home directory itself with `~`. Errors print `ERR:` and exit 1; successful commands and clean shutdown exit 0.

Configure TOML `version = 1`, named `[projects.<name>]` tables with a required `path`, and ordered `[[overlays]]` tables with unique `name`, required `source`, required `projects` glob selectors, and optional `glob` (default `**/*`). `[defaults]` supplies `collision = "concat"` and `write = "most-specific"`; per-overlay fields inherit them. These are the supported policies. Use relative forward-slash globs without `.` or `..` components; recursive globs include dotfolders and selected ancestors. Reject unknown TOML fields, invalid patterns, overlapping mount targets, and sources inside their mount targets.

Read regular files as project bytes followed by matching overlay bytes in declaration order. Add no separators, including for binary files. Merge directories recursively at every depth. Type collisions yield I/O errors; single symlinks retain their targets. Keep `.git` supplied exclusively by the backing project. Store skills in source `.agents/skills/` directories.

Write project-only files into the backing project. Write overlay files into their most-specific contribution. For concatenated full-document saves, preserve the earlier contributions exactly at the beginning; strip that prefix when saving the final layer. Support direct writes, append, and atomic saves. Stage merged writes until flush, fsync, or close; reject changed prefixes with a permission error. Truncating to zero clears only the final contribution. Opened merged files retain snapshots; subsequent opens use current sources.

Create entries in the project when the parent has project backing, otherwise in its most-specific overlay directory. Restricted globs must include new overlay paths. Reject unlinking overlay contributions, removing overlay directories, and moving existing overlay paths away. Permit newly created editor staging files to replace existing overlay documents atomically. Edit source directories directly to delete or move their files. Stop overlays before recursively deleting a worktree; Git removal through a mount encounters protected paths. Directory moves across layers, links to concatenated files, rename flags, special-file creation, and native extended attributes are unsupported.

Watch native Git common metadata and overlay source directories with fsnotify, debounce relevant events for 200 ms, and reconcile every 30 seconds as a fallback. Discover checkout locations with `git worktree list --porcelain -z`; all worktrees use the original configured sources without local `.ai` links. Source additions, removals, and newly created nested directories update the visible path index.

Temporarily add uniquely marked blocks to Git `info/exclude` for selected overlay paths absent from project backing. Keep tracked/backing collisions visible to Git, preserve user entries, and remove each mount's block on shutdown. Worktrees share their repository's exclude file. Record mount targets and exact managed blocks under `<config-directory>/.tmp/workspace-overlay/`; use an advisory owner lock and atomic registry replacement. After forced termination, use `overlay unmount` or `overlay mount --replace` to recover. Retain incomplete cleanup records; changed managed blocks cause an error for manual resolution.

From the sample workspace, run `just dev`, `just status`, and `just stop`; optional flags pass through, for example `just dev --project alpha`. `just dev` supplies `--replace` and defaults to all projects and worktrees.
