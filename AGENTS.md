# workspace-overlay-cli

Go FUSE CLI tool that serves layered workspace overlays over project backing directories.

## Commands
- Build: `just build` (writes `bin/workspace-overlay` with source paths removed)
- Dev: `just dev [--project name]` (shared fixtures in `.tmp/dev-workspace`, all projects and worktrees by default)
- Stop: `just stop [--project name]`
- Status: `just status [--project name]`
- Test: `just test` (race detector plus the 90% coverage gate); `go test -race -cover ./...` runs the tests without the gate.
- Lint: `go vet ./... && go mod tidy -diff` or `just lint`
- Run: `go run ./cmd/workspace-overlay <args>` or `just run <args>`
- Run AI: `AGENT=1 go run ./cmd/workspace-overlay <args>` or `just run-ai <args>`

## Setup
- Requires Linux with accessible `/dev/fuse` and the `git`, `findmnt`, and `fusermount3` commands on `PATH`.
- Pre-mount directory handles (`os.OpenRoot`) keep the backing project accessible under the mount.
- Copy `workspace-overlay.example.toml` into the workspace as `workspace-overlay.toml` and adjust its project and source paths. Runtime configuration and overlay data belong to that workspace.

## Conventions
- Layout: `cmd/workspace-overlay` only wires commands and embeds `SKILL.md`. Code lives in `internal/` by responsibility: `config` (TOML), `overlayfs` (merged view and FUSE nodes), `session` (mounting, worktrees, recovery), `registry` (state files), `gitexclude`, `fixture`, `pathname`, `gitrepo`, `logged`. `internal/scratch` is test support and is excluded from the coverage gate.
- Module path: `github.com/alexgorbatchev/workspace-overlay-cli`. Group imports as standard library, third-party, then this module (`goimports -local github.com/alexgorbatchev/workspace-overlay-cli`).
- `overlayfs` tests mount a view directly with `View.Mount`; only `session` tests go through `Mount`, the registry and Git exclusions.
- Keep README usage focused on operating the background utility; omit sample terminal output blocks.
- Command hierarchy uses subject-first noun-verb structure (`workspace-overlay overlay <mount|unmount|status>`).
- Embedded skill contract: `workspace-overlay skill` prints `cmd/workspace-overlay/SKILL.md` verbatim; keep `SKILL.md` synchronized in the same change as any command, flag, default, environment, output, or side-effect change.
- Dual-mode output: `AGENT=1` prefixes help screens with `ALERT: Agents must read \`AGENT=1 workspace-overlay skill\` before using this tool.`.
- Merge project backing bytes first, then matching overlays in TOML declaration order. Only text files are joined end-to-end; when a path has copies in multiple layers and at least one is binary, reading returns an explanation of the collision and writes are rejected. Merge directories recursively. Put source skills in `.agents/skills/`.
- Writable overlay: edits persist into the most-specific contributing layer; earlier contributions must remain intact. Atomic editor save-and-rename strips prefix bytes in `prepareSave` (`internal/overlayfs`).
- Dynamic Git exclusions: overlay-only files are recorded in `.git/info/exclude` under managed marker blocks using advisory file locks (`syscall.Flock`).
- Overlay source files and directories cannot be deleted through project or worktree mounts. Preserve ordinary project deletion and writable overlay edits, including atomic editor saves.
- State files (mount registry, owner lock, stop request) live under `$XDG_STATE_HOME/workspace-overlay/` (default `~/.local/state/workspace-overlay/`) and are removed on a clean stop. The owner deletes its lock file while still holding the lock, and `registry.Acquire` retries when the file it locked is no longer the one on disk; every test package calls `scratch.Main` from `TestMain`, which keeps `TMPDIR` inside `.tmp/` and gives each test process its own `XDG_STATE_HOME` under `.tmp/state-*`, removed when the process exits and shared with child processes it starts.
- Entries created through a mount inside an overlay directory stay removable for that mount session while existing overlay sources stay protected. Atomic editor saves write to a temporary name and rename over the final layer.
- Unsupported worktrees (inside another mount target, containing an overlay source, or already mounted) are skipped with a message on stderr and retried after unregistration and re-registration; projects and other worktrees keep running.
- `run(args, stdout, stderr)` is the testable entrypoint; tests must not mutate `os.Args`, `os.Stdout`, or the environment except through `t.Setenv`. `TestGuideDocumentsEveryCommandAndFlag` fails when `SKILL.md` misses a command or flag.
- Configure ordered overlays and projects through `workspace-overlay.toml`; `--config` selects a file, otherwise discover it in ancestors of the current directory.
- Watch Git common metadata and overlay sources through fsnotify using native backing directory handles. Reconcile registered worktrees after debounced events and use a slow fallback scan for missed events.
- The user approved go-fuse/v2, Cobra, cobra-help-tree/v2, fsnotify, go-toml/v2, and doublestar dependencies.
- Keep the repository self-contained; integration tests create temporary projects and worktrees instead of relying on sibling fixtures. `internal/session/example_config_test.go` exercises the shipped configuration with mounted reads and writes.
- Keep the CLI checkout at its current location until the user requests relocation.
- Generate live verification projects through `fixture create`; share embedded fixture data with integration tests and retain edits across restarts. Use mock names `alpha`, `beta`, and `workspace` in fixtures and examples.

## Gotchas
- Mount replacement: an active overlay requires `--replace` to unmount and remount cleanly; refusing to unmount non-overlay filesystems.
- Worktrees share Git metadata: all backing and Git exclude handles are opened before mounting any target.
- Worktrees use the original configured overlay sources; do not create per-worktree `.ai` symlinks.
- Home directory paths: paths inside `~` are abbreviated with `~/` in status and mount output.
- Shared memory mapping: a test that maps a mounted file into memory must do so from a child process, because a page fault on a mount served by the same process can deadlock it (see `TestSharedMappingOfProjectFile`).
- Session tests never wait for the watcher with a fixed sleep: `awaitReconcile` in `internal/session/watcher_test.go` registers a worktree and waits for its mount, which proves a reconcile ran after the call.
- Git tracking: a file that Git tracks in the project and that also has an overlay contribution reads as the joined content, so Git reports it as modified, and Git operations that rewrite it (checkout, stash, restore, pull) fail while mounted.

## Live verification
- Run `just dev` to create or reuse `.tmp/dev-workspace` and mount `alpha`, `beta`, and `.workspaces/one/{alpha,beta}` within that workspace. Use `just status` and `just stop` from another terminal. Optional `--project alpha` selects one project.
- Integration tests use `fixture.Create` and the embedded `internal/fixture/testdata/workspace/` data, including the shipped example configuration. Fixture Git commands disable hooks and signing and exclude inherited `GIT_*` overrides.
- Append to `.tmp/dev-workspace/alpha/AGENTS.md`; verify the final contribution in `.tmp/dev-workspace/.ai/alpha/AGENTS.md` and a fresh read from `.tmp/dev-workspace/.workspaces/one/alpha/AGENTS.md`. Nested colliding notes are at `docs/nested/notes.md`; shared and project skills appear in `.agents/skills/`.
- Add another worktree while mounted with `git -C .tmp/dev-workspace/alpha worktree add -b live-check ../.workspaces/two/alpha main`; `just status` should include it after watcher reconciliation. Stop overlays before `git worktree remove`.
- Runtime fixture files are ignored under `.tmp/`. Stop removes managed exclusions and mounts; retain fixture edits and Git history for subsequent starts. Failed initialization retains an incomplete directory and refuses reuse rather than overwriting it.
- For a real workspace use `just run overlay mount --config path/to/workspace-overlay.toml` instead of the fixture recipes.

## Boundaries
- Always: automatically record all new instructions in the most appropriate `AGENTS.md` file immediately upon receipt (check with user if existing instructions conflict)
- Always (code-based projects only): any time code is changed such that results from running that code are changed, a test file must be changed as well; 90% code coverage is required (scripts/ folder is excluded from this rule)
- Always: keep `cmd/workspace-overlay/SKILL.md` synchronized in the same change as any CLI command, flag, default, environment, output, or side-effect change
- Ask first: modifying FUSE options, external dependency changes, or adding root flags
- Never: publish releases, tags, packages, or production deployments automatically without explicit user authorization
- Never: unmount non-overlay filesystems (`fuse.workspace-overlay` only)
- Never: commit compiled binaries in `bin/`
