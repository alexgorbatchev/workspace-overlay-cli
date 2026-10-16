---
created_on: 2026-10-16 21:22
last_modified: 2026-10-16 21:27
status: current
ticket_status: closed
---

# Wave 1: Keep mount backing handles alive through reconciliation

## Problem

Mount workers close their views before the session consumes their completion events. Reconciliation can select a watcher event first and access those closed views. Required landing tests fail with `statat .: file already closed` and `openat exclude: file already closed`.

## Why this matters

Hand-unmounting a worktree can stop its entire project session. This failure blocks landing the directory attribute caching and configuration documentation changes.

## Observed context

- Reproduced by `just test` on untouched base `53bc4b4`: `TestWorktreeRegisteredAgainIsMountedAgain` and `TestLiveWorktreeDiscovery` fail in agent mode.
- `internal/session/project.go` closes worker-owned handles while the completed mount remains in `projectRunner.active`.
- `internal/session/project_test.go` exercises reconciliation before consuming completion.

## Dependencies

- **Prerequisites (Depends on):** None.
- **Blocks:** Landing directory attribute caching and overlay scope documentation on `main`.

## Desired outcome

The session owns backing-handle cleanup and performs it when removing completed mounts from its active set, preserving shutdown errors, Git exclusions, and registry cleanup.

## Acceptance criteria

- [x] Add a deterministic real-mount regression that fails against the unchanged base implementation.
- [x] Retain handles until completion is consumed, including reconciliation and shutdown paths.
- [x] Verify the regression fails with the repair temporarily disabled.
- [x] Pass dual-mode race tests with at least 90% coverage, lint, and build.
- [x] Run a separate review pass and resolve its findings.
