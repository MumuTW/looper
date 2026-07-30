#!/bin/sh
# Build looperd from a git ref and stage it next to the installed daemon.
#
# The one rule this script exists to enforce: it never writes the binary of a
# looperd that is currently running. Overwriting a live daemon's executable is
# how looper interrupts itself — an agent working on this repo rebuilt and
# installed looperd, the daemon was restarted onto the new build, and every
# in-flight agent died with it (issue #154). Staging is the default and
# promotion is a separate, deliberate act performed against a stopped daemon.
#
#   sh scripts/update-daemon.sh              # build + stage, never touch the live binary
#   sh scripts/update-daemon.sh --promote    # install the staged build (daemon must be stopped)
#
#   REPO=/path/to/checkout  REF=some-branch  BIN=/path/to/looperd   # overrides
#
# This script does not start, stop, signal, or `launchctl kickstart` anything.
# Restarting the daemon is the operator's call, because it drops in-flight runs.

set -eu

script_dir="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
REPO="${REPO:-$(CDPATH='' cd -- "$script_dir/.." && pwd)}"
REF="${REF:-origin/main}"
BIN="${BIN:-$HOME/.looper/bin/looperd}"

promote=0
for arg in "$@"; do
  case "$arg" in
    --promote) promote=1 ;;
    -h|--help)
      sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      printf 'error: unknown argument: %s\n' "$arg" >&2
      exit 2
      ;;
  esac
done

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

# pids_executing reports which processes are running the file at $1.
#
# An inode-level answer is the point: after `cp` the path is the same file, so
# "is anything executing this exact binary" is the question, not "does the path
# look like the daemon's". Without lsof the answer is unknown, and unknown must
# refuse — a wrong "nothing is running it" is exactly the failure this guards.
pids_executing() {
  target="$1"
  [ -e "$target" ] || return 0
  command -v lsof >/dev/null 2>&1 || fail "lsof is required to prove no daemon is running $target; refusing to install blind"
  lsof -t -- "$target" 2>/dev/null || true
}

cd "$REPO"

git fetch --quiet origin || true
sha="$(git rev-parse "$REF")"
short="$(git rev-parse --short=7 "$REF")"
staged="$BIN.staged-$short"

mkdir -p "$(dirname "$BIN")"

if [ "$promote" -eq 0 ]; then
  # Build from a detached worktree so a dirty or checked-out repo is never touched.
  tmp="$(mktemp -d)"
  trap 'git worktree remove --force "$tmp" 2>/dev/null || true; rm -rf "$tmp"' EXIT
  git worktree add --quiet --detach "$tmp" "$sha"

  printf 'building %s -> %s\n' "$REF" "0.0.0-dev+g$short"
  (
    cd "$tmp"
    LOOPER_BUILD_GIT_SHA="$sha" \
    LOOPER_BUILD_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    LOOPER_BUILD_VERSION="0.0.0-dev+g$short" \
    ./scripts/build-looperd.sh --output "$tmp/looperd.new"
  )

  built="$("$tmp/looperd.new" --version)"
  [ "$built" = "0.0.0-dev+g$short" ] || fail "built version $built does not match expected 0.0.0-dev+g$short"

  cp "$tmp/looperd.new" "$staged"
  chmod 0755 "$staged"

  printf 'staged %s (%s)\n' "$staged" "$built"
  printf '\nThe live binary at %s was not touched.\n' "$BIN"
  printf 'To install it, stop looperd first, then:\n'
  printf '  sh scripts/update-daemon.sh --promote\n'
  printf 'Promoting while looperd runs is refused: the restart that follows drops every in-flight agent run.\n'
  exit 0
fi

[ -f "$staged" ] || fail "no staged build for $REF at $staged; run this script without --promote first"

running="$(pids_executing "$BIN")"
if [ -n "$running" ]; then
  fail "$(printf 'refusing to overwrite %s: pid(s) %s are executing it.\nStop looperd first. Installing over a running daemon replaces the build an operator chose, and the restart that follows kills every in-flight agent run (#154).' "$BIN" "$(printf '%s' "$running" | tr '\n' ' ')")"
fi

if [ -f "$BIN" ]; then
  cp "$BIN" "$BIN.bak-$(date +%Y%m%d-%H%M%S)"
fi
cp "$staged" "$BIN"
chmod 0755 "$BIN"
rm -f "$staged"

printf 'installed %s (%s)\n' "$BIN" "$("$BIN" --version)"
printf 'looperd is not running; start it when you are ready.\n'
