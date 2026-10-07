---
name: workspace-overlay
description: Use when mounting, inspecting, or stopping workspace-overlay and editing its merged project view.
author: alexgorbatchev
metadata:
  created_on: 2026-10-06 16:00
  last_modified: 2026-10-06 20:13
  status: current
---

Run commands from `.poc/workspace-overlay-cli` for the relative defaults below.

- `workspace-overlay overlay mount`: Serve a writable merged project in the foreground. Accept no positional arguments. `--root` (string, default `..`) selects the workspace directory relative to the current directory. `--workspace` (string, default `workspace`) selects the shared `.ai` layer. `--project` (string, default `alpha`) selects the project directory and its `.ai` layer. Names must be single directory components and must differ. Require all three directories to exist. Mount over `<root>/<project>`, preserving its backing files through pre-mount directory handles. Send SIGINT or SIGTERM to unmount; external unmount also ends the process.
- `workspace-overlay overlay mount --replace`: `--replace` (bool, default false) unmounts an existing `fuse.workspace-overlay` mount first. Refuse to unmount any other filesystem type.
- `workspace-overlay overlay unmount`: Unmount the project overlay, or succeed if already unmounted. Accept no positional arguments. `--root` (string, default `..`) and `--project` (string, default `alpha`) select `<root>/<project>`. Refuse other filesystem types.
- `workspace-overlay overlay status`: Print the mounted filesystem type, or `unmounted`, to stdout. Accept no positional arguments. `--root` (string, default `..`) and `--project` (string, default `alpha`) select `<root>/<project>`. An unmounted directory returns exit 0.
- `workspace-overlay skill`: Print this embedded reference verbatim, offline, without arguments or command-specific flags.
- `workspace-overlay help [command]`: Print help for the selected command path.
- `workspace-overlay --version`: Print `poc` followed by a newline.
- `--help` / `-h` (bool, default false): Print help on any command. `--version` / `-v` (bool, default false) is available on the root. Completion generation is disabled.

Set `AGENT=1`, `true`, or `yes` for compact help with a skill-reading alert; case and surrounding whitespace are ignored. Mount progress and file-resolution diagnostics go to stderr. Mount messages abbreviate paths within the user's home directory with `~/`; the home directory itself displays as `~`. Other paths remain unchanged. Errors print `ERR:` and exit 1; successful help, skill, version, and clean shutdown exit 0.

Merge matching relative paths recursively at every depth. Concatenate all colliding regular files byte-for-byte in order: existing project, shared `.ai/<workspace>`, project `.ai/<project>`. Add no separators or generated headers; all file types use their source bytes. Merge directory listings, retaining names that occur in only one layer. A file/directory mismatch or colliding non-regular files yields an I/O error. Single symlinks retain their targets. Special-file opens are unsupported.

For a project with its own `.git` directory or worktree metadata file, mount temporarily adds a uniquely marked block to Git's `info/exclude` file. Ignore only overlay paths absent from the backing project; backing files, including concatenated collisions, remain visible to Git. Refresh exclusions when listing directories or inspecting project-root/Git metadata. Preserve other exclude entries and remove this mount's block after unmount or clean shutdown, including failed mounts. Projects without `.git` skip this step. Git worktrees share the common repository's exclude file. Forced termination such as SIGKILL cannot run cleanup; files tracked by Git are unaffected by exclusion rules.

Write ordinary project files directly into their backing project. Write overlay and concatenated files into their most-specific contributing layer. For a full-document save, retain the earlier contributions byte-for-byte at the beginning; the CLI strips that prefix before saving the final contribution. Direct saves, append, and temporary-file-plus-rename saves follow this rule. Writes to concatenated files are staged until close/flush or fsync. Changes to earlier contributions are rejected with a permission error; edit their `.ai` sources directly. Truncating to zero clears only the final contribution. Edit sources to update subsequent opens; opened merged files retain snapshots.

Create new entries in the project if the parent has a project backing directory; otherwise use its most-specific overlay directory. Removing an overlay file removes its final contribution and exposes any earlier contributions. Renaming/removing a directory spread across layers and hard-linking a concatenated file are unsupported. Native extended attributes and special-file creation are unsupported. Native rename flags are unsupported; ordinary replacement renames work.

Mount with `workspace-overlay overlay mount`, then read `../alpha/AGENTS.md` or `../alpha/docs/deep/nested/AGENTS.md` from another terminal. Linux requires accessible `/dev/fuse` and a FUSE mount helper. Mount replacement, status, and unmount use `findmnt` internally; explicit unmount uses `fusermount3`. This POC is verified on Linux only.

From `.poc/`, use `just dev [project]`, `just stop [project]`, and `just status [project]` (default project: alpha). Each invocation manages one project; each project gets the same workspace layer and its own project layer. Run separate mount processes for multiple projects.
