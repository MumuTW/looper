#!/usr/bin/env bash
# Per-repo Hermes profile for looper.
#
# Hermes has no per-repo profile discovery: a profile is just an independent
# HERMES_HOME directory, selected by exporting HERMES_HOME (the `looper`
# profile lives under ~/.hermes/profiles/looper). This script is the repo's
# checked-in definition of that selection, so every clone drives the same
# profile with the same Devin ACP backend instead of the developer's default
# profile.
#
# The profile itself (config.yaml, .env, SOUL.md, memories/) is user state and
# is NOT checked in. `--bootstrap` creates it from scratch when missing.
#
# Usage:
#   source scripts/hermes-profile.sh      # export HERMES_HOME into this shell
#   scripts/hermes-profile.sh --bootstrap # create/repair the profile, then exit
#   scripts/hermes-profile.sh --print     # print the resolved HERMES_HOME
#
# SAFETY: a Hermes session on this backend can read and write the directory it
# runs in. Hermes's ACP shim services fs/write_text_file directly, with no
# permission round-trip, and in the default agent type Devin also runs its own
# tools in its own loop. Neither side is sandboxed here. Run it from a
# disposable worktree, not from a checkout you care about.
#
# Backend evidence and known gaps: docs/research/hermes-devin-acp-spike.md

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  SOURCED=0
  # Strict mode only when executed. When sourced, `set -e` would leak into the
  # caller's interactive shell and kill it on the next failing command.
  set -euo pipefail
else
  SOURCED=1
fi

FORCE=0
# Absolute, so the printed commands stay valid after the reader cd's into the
# disposable directory the safety note tells them to use.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

HERMES_ROOT="${HERMES_ROOT:-$HOME/.hermes}"
# Baked into the printed MCP registration: Devin spawns that server in a future
# session with only the env recorded at registration time, so an install dir
# that is only set in this shell would be lost and the server would fall back
# to the default path and fail every call with an import error.
HERMES_INSTALL_DIR="${HERMES_INSTALL_DIR:-$HOME/.hermes/hermes-agent}"
LOOPER_HERMES_PROFILE="${LOOPER_HERMES_PROFILE:-looper}"
LOOPER_HERMES_HOME="$HERMES_ROOT/profiles/$LOOPER_HERMES_PROFILE"
LOOPER_DEVIN_MODEL="${LOOPER_DEVIN_MODEL:-glm-5-2}"
# Deliberately narrow: the memory writers only. Widening this grants the ACP
# backend unattended use of whatever you add.
# Fully-qualified MCP names (mcp__<server>__<tool>) — the form Devin reports.
# Pinning the server too means another server exposing a same-named tool does
# not inherit the grant.
LOOPER_MCP_SERVER="${LOOPER_MCP_SERVER:-hermes-memory}"
LOOPER_ALLOWED_TOOLS="${LOOPER_ALLOWED_TOOLS:-}"
if [ -z "$LOOPER_ALLOWED_TOOLS" ]; then
  for _t in hermes_memory_add hermes_memory_replace hermes_memory_remove hermes_memory_read; do
    LOOPER_ALLOWED_TOOLS="${LOOPER_ALLOWED_TOOLS:+$LOOPER_ALLOWED_TOOLS,}mcp__${LOOPER_MCP_SERVER}__${_t}"
  done
fi

# Writes $1 from stdin, but never over an existing file: profile config.yaml
# and .env hold the user's own provider credentials and settings, and this
# script does not own them. An existing file is left alone and reported, or
# backed up first when --force was passed.
write_profile_file() {
  local path="$1" label="$2" content
  content="$(cat)"

  if [ ! -e "$path" ]; then
    printf '%s' "$content" > "$path"
    echo "  wrote $label"
    return 0
  fi

  if [ "$(cat "$path")" = "$content" ]; then
    echo "  $label already current"
    return 0
  fi

  if [ "$FORCE" -eq 1 ]; then
    local backup="$path.bak-$(date +%Y%m%d_%H%M%S)"
    cp "$path" "$backup"
    printf '%s' "$content" > "$path"
    echo "  replaced $label (previous contents saved to $(basename "$backup"))"
    return 0
  fi

  echo "  SKIPPED $label — it already exists and differs from the template." >&2
  echo "    Left untouched; it may hold your own credentials or settings." >&2
  echo "    Merge by hand, or re-run with --force to replace it after a backup." >&2
  SKIPPED=1
  return 0
}

bootstrap() {
  SKIPPED=0
  if [ ! -d "$LOOPER_HERMES_HOME" ]; then
    # Anchor creation at the configured root, or a custom HERMES_ROOT would
    # create the profile under Hermes's default home and leave the path this
    # script then writes to nonexistent.
    HERMES_HOME="$HERMES_ROOT" hermes profile create "$LOOPER_HERMES_PROFILE" --no-alias \
      --description "Looper repo profile: Devin ACP backend, repo-scoped memory"
  fi

  write_profile_file "$LOOPER_HERMES_HOME/config.yaml" "config.yaml" <<YAML
model:
  default: copilot-acp
  provider: copilot-acp
agent:
  max_turns: 60
  # 1 = single attempt. Devin's free tier rate-limits aggressively and each
  # retry burns another unit of the same quota, so retrying deepens the hole.
  api_max_retries: 1
memory:
  memory_enabled: true
  user_profile_enabled: true
YAML

  write_profile_file "$LOOPER_HERMES_HOME/.env" ".env" <<ENV
# Devin CLI as the ACP model backend. Hermes's copilot-acp provider speaks
# generic ACP v1; these two vars repoint it at \`devin acp\` with no patch.
HERMES_COPILOT_ACP_COMMAND=devin
HERMES_COPILOT_ACP_ARGS=acp --model $LOOPER_DEVIN_MODEL

# Tools the ACP permission gate may approve, one call at a time. Only takes
# effect with tools/hermes-devin/apply-hermes-patch.sh applied; stock Hermes
# denies everything regardless. Keep this list minimal — each name here is a
# tool the backend can invoke without a human in the loop.
HERMES_ACP_ALLOWED_MCP_TOOLS=$LOOPER_ALLOWED_TOOLS
ENV

  echo "Hermes profile '$LOOPER_HERMES_PROFILE' at $LOOPER_HERMES_HOME"
  echo "Model backend: devin acp --model $LOOPER_DEVIN_MODEL"
  echo "Memory lives in $LOOPER_HERMES_HOME/memories/ (separate from your default profile)"
  if [ "$SKIPPED" -eq 1 ]; then
    echo
    echo "One or more files were left as-is (see above) — the profile may not"
    echo "be configured for the Devin backend until you reconcile them."
  fi
  echo
  echo "Memory WRITES additionally need, once each:"
  echo "  $REPO_ROOT/tools/hermes-devin/apply-hermes-patch.sh"
  echo "  devin mcp add $LOOPER_MCP_SERVER \\"
  echo "    -e HERMES_HOME=$LOOPER_HERMES_HOME \\"
  echo "    -e HERMES_INSTALL_DIR=$HERMES_INSTALL_DIR \\"
  echo "    -- $REPO_ROOT/tools/hermes-devin/memory_mcp_server.py"
  echo "Run the devin command once from this repo root: it writes"
  echo ".devin/mcp_config.local.json there (gitignored — it holds absolute"
  echo "paths), and Devin walks up from a session's cwd to find it, so"
  echo "subdirectories are covered. Sessions outside this tree are not."
  echo "Without both steps, recall still works and writes silently no-op."
}

# When sourced, $@ is the CALLER's argument list unless the caller passed
# arguments to `source` explicitly. Parsing it would let an unrelated caller
# argument select a mode here — and `exit` would terminate the caller's shell
# rather than this script. So only parse when executed.
if [ "$SOURCED" -eq 0 ]; then
  case "${1:-}" in
    --bootstrap) ;;
    --force)     FORCE=1 ;;
    --print)     echo "$LOOPER_HERMES_HOME"; exit 0 ;;
    --help|-h)
      echo "usage: hermes-profile.sh [--bootstrap [--force] | --print]"
      echo "       source hermes-profile.sh    # export HERMES_HOME for this shell"
      exit 0
      ;;
    "")
      echo "Nothing to do when executed without a flag." >&2
      echo "Did you mean:  source ${BASH_SOURCE[0]}" >&2
      exit 2
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
  if [ "${2:-}" = "--force" ]; then
    FORCE=1
  fi
  bootstrap
  exit 0
fi

if [ ! -d "$LOOPER_HERMES_HOME" ]; then
  echo "Hermes profile '$LOOPER_HERMES_PROFILE' does not exist." >&2
  echo "Run: $REPO_ROOT/scripts/hermes-profile.sh --bootstrap" >&2
  return 1
fi

export HERMES_HOME="$LOOPER_HERMES_HOME"
