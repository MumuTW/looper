#!/bin/sh

# One-line PR takeover bootstrap.
#
# Installs the looper CLI if it is missing, then hands off to
# `looper takeover`, which installs/starts the managed daemon, registers the
# repository scoped to a single pull request, and runs the reviewer + fixer
# loops until the PR is approved and merged.
#
# Intended to be pasted into a PR comment, for example:
#
#   curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/takeover.sh \
#     | sh -s -- acme/repo#42
#
# Everything after `--` is forwarded verbatim to `looper takeover`, so flags
# work too:
#
#   ... | sh -s -- acme/repo#42 --merge --agent-vendor claude-code
#
# Run this from inside a checkout of the repository: looper takeover resolves
# the repo root, and (when no <owner/repo>#<number> is given) the current
# branch's PR, from the working directory.

set -eu

OWNER="${LOOPER_GITHUB_OWNER:-nexu-io}"
REPO="${LOOPER_GITHUB_REPO:-looper}"
REF="${LOOPER_INSTALL_REF:-main}"

log() {
  printf '%s\n' "$*"
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

# Resolve a runnable looper binary, installing the CLI when it is absent.
resolve_looper() {
  if command -v looper >/dev/null 2>&1; then
    command -v looper
    return 0
  fi

  install_dir="${LOOPER_INSTALL_DIR:-$HOME/.local/bin}"
  if [ -x "$install_dir/looper" ]; then
    printf '%s\n' "$install_dir/looper"
    return 0
  fi

  log "looper CLI not found; installing it..." >&2
  need_cmd curl
  curl -fsSL "https://raw.githubusercontent.com/$OWNER/$REPO/$REF/scripts/install.sh" | sh >&2

  if command -v looper >/dev/null 2>&1; then
    command -v looper
    return 0
  fi
  if [ -x "$install_dir/looper" ]; then
    printf '%s\n' "$install_dir/looper"
    return 0
  fi
  fail "looper CLI was installed but is not on PATH; open a new shell or run: export PATH=\"$install_dir:\$PATH\""
}

need_cmd git
need_cmd gh

looper_bin="$(resolve_looper)"

log "Starting takeover with: $looper_bin takeover $*"
exec "$looper_bin" takeover "$@"
