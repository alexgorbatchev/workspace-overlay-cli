#!/usr/bin/env bash
set -euo pipefail

mkdir -p .tmp
run_dir=$(mktemp -d "$PWD/.tmp/test.XXXXXX")
run_dir=$(cd "$run_dir" && pwd -P)
test_pid=
cleanup() {
  if [[ -n "$test_pid" ]]; then
    kill -TERM -- "-$test_pid" 2>/dev/null || true
    # Bound graceful shutdown before removing files still used by test children.
    for ((attempt = 0; attempt < 50; attempt++)); do
      kill -0 -- "-$test_pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL -- "-$test_pid" 2>/dev/null || true
  fi
  # Killed FUSE servers leave mounts behind. Only detach this run's overlays;
  # never traverse a dead mount or unmount another session's filesystem.
  local mounts target filesystem
  mounts=$(findmnt --raw --noheadings --output TARGET,FSTYPE) || exit 1
  while read -r target filesystem; do
    printf -v target '%b' "$target"
    if [[ "$target" == "$run_dir/"* && "$filesystem" == fuse.workspace-overlay ]]; then
      fusermount3 -u -z -- "$target" || exit 1
    fi
  done <<< "$mounts"
  if [[ -n "$test_pid" ]]; then
    wait "$test_pid" 2>/dev/null || true
  fi
  rm -rf --one-file-system "$run_dir"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

agent=${1:?test mode is required}
coverage_packages=$(go list ./... | awk '!/\/internal\/scratch$/' | paste -sd, -)

# Job control gives the test command its own process group on Linux and macOS.
set -m
AGENT="$agent" TMPDIR="$run_dir" GOTMPDIR="$run_dir" go test -race -count=1 -coverpkg="$coverage_packages" -coverprofile="$run_dir/coverage.out" ./... &
test_pid=$!
wait "$test_pid"
set +m

go tool cover -func="$run_dir/coverage.out" | awk '
  /^total:/ {
    found = 1
    sub("%", "", $NF)
    printf "Coverage: %s%%\n", $NF
    if ($NF+0 < 90) exit 1
  }
  END { if (!found) { print "No statement coverage data found"; exit 1 } }
'
