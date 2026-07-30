#!/usr/bin/env bash
# Apply the carried ACP permission allow-list patch to a local Hermes install.
#
# Hermes's ACP shim (agent/copilot_acp_client.py) denies every
# session/request_permission, so a Devin ACP backend can never complete an MCP
# tool call. This patch makes that denial selective: tools named in
# HERMES_ACP_ALLOWED_MCP_TOOLS get single-call approval, everything else keeps
# the stock behaviour. With the env var unset the patched file behaves exactly
# like stock Hermes.
#
# This is a CARRIED patch against an unsupported integration — Hermes upstream
# knows nothing about it. It is pinned to one exact upstream file (Hermes
# v0.19.0 / upstream tag v2026.7.20). If Hermes is updated underneath it, this
# script refuses rather than half-applying: a partially patched permission
# gate is far worse than no patch.
#
# Usage:
#   tools/hermes-devin/apply-hermes-patch.sh            # apply
#   tools/hermes-devin/apply-hermes-patch.sh --revert   # restore stock
#   tools/hermes-devin/apply-hermes-patch.sh --status   # report current state
#
# Env:
#   HERMES_INSTALL_DIR  Hermes install root (default: ~/.hermes/hermes-agent)
#
# SAFETY: the shim also services fs/write_text_file with no permission
# round-trip, so this file is already the weakest point in the sandbox story.
# Keep HERMES_ACP_ALLOWED_MCP_TOOLS as narrow as the task needs, and run
# Hermes from a disposable worktree. See scripts/hermes-profile.sh.
set -euo pipefail

HERMES_INSTALL_DIR="${HERMES_INSTALL_DIR:-$HOME/.hermes/hermes-agent}"
TARGET_REL="agent/copilot_acp_client.py"
TARGET="$HERMES_INSTALL_DIR/$TARGET_REL"
BACKUP="$TARGET.orig"
PATCH_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/acp-permission-allowlist.patch"

# sha256 of the exact upstream file this patch was generated against, and of
# the result of applying it. Anything else means "do not touch".
STOCK_SHA="03190fcd4f9c985cab5cbaa90f7391cad8122148d7d300a81da8be0c2189c4bf"
PATCHED_SHA="4ec44e3260a9d86bba91e19a3dd8dc1d9f793e341855df19fd876b122bac1517"

die() { echo "$*" >&2; exit 1; }

sha_of() { shasum -a 256 "$1" | cut -d' ' -f1; }

# Prints one of: patched | stock | unknown
target_state() {
  local sha
  sha="$(sha_of "$TARGET")"
  case "$sha" in
    "$PATCHED_SHA") echo patched ;;
    "$STOCK_SHA")   echo stock ;;
    *)              echo unknown ;;
  esac
}

preflight() {
  [ -f "$PATCH_FILE" ] || die "missing patch file: $PATCH_FILE"
  [ -d "$HERMES_INSTALL_DIR" ] || die "Hermes install not found: $HERMES_INSTALL_DIR (set HERMES_INSTALL_DIR)"
  [ -f "$TARGET" ] || die "target file not found: $TARGET"
  command -v patch >/dev/null 2>&1 || die "missing required tool: patch"
  command -v shasum >/dev/null 2>&1 || die "missing required tool: shasum"
}

refuse_unknown() {
  cat >&2 <<MSG
refusing to touch $TARGET

Its contents match neither the upstream file this patch was built against nor
the patched result. Hermes was most likely updated, or the file was edited by
hand. Applying anyway could leave the permission gate half-rewritten.

  expected (stock)   $STOCK_SHA
  expected (patched) $PATCHED_SHA
  actual             $(sha_of "$TARGET")

Regenerate the patch against the current Hermes before continuing.
MSG
  exit 1
}

do_apply() {
  preflight
  case "$(target_state)" in
    patched) echo "already applied: $TARGET"; return 0 ;;
    unknown) refuse_unknown ;;
  esac

  # An existing .orig is not ours to overwrite: it may be the only copy of a
  # shim the operator customized, left by an earlier manual patch or a failed
  # recovery. Side-step to a timestamped name rather than clobbering it.
  if [ -e "$BACKUP" ]; then
    BACKUP="$TARGET.orig-$(date +%Y%m%d_%H%M%S)"
    echo "note: $TARGET.orig already exists and was left untouched;"
    echo "      this run's backup goes to $(basename "$BACKUP")"
  fi

  cp -p "$TARGET" "$BACKUP"
  patch -p1 -s -d "$HERMES_INSTALL_DIR" < "$PATCH_FILE" || {
    cp -p "$BACKUP" "$TARGET"
    die "patch failed; restored $TARGET from $BACKUP"
  }

  if [ "$(target_state)" != "patched" ]; then
    cp -p "$BACKUP" "$TARGET"
    die "patched file did not match the expected checksum; restored from $BACKUP"
  fi

  python3 -c "import ast, sys; ast.parse(open(sys.argv[1]).read())" "$TARGET" || {
    cp -p "$BACKUP" "$TARGET"
    die "patched file does not parse as Python; restored from $BACKUP"
  }

  echo "✓ applied: $TARGET (backup: $BACKUP)"
  echo "  set HERMES_ACP_ALLOWED_MCP_TOOLS=tool_a,tool_b to opt tools in; unset = deny all"
}

do_revert() {
  preflight
  case "$(target_state)" in
    stock)   echo "not applied: $TARGET is already stock"; return 0 ;;
    unknown) refuse_unknown ;;
  esac

  patch -R -p1 -s -d "$HERMES_INSTALL_DIR" < "$PATCH_FILE" || die "revert failed: $TARGET left as-is"

  [ "$(target_state)" = "stock" ] || die "revert did not restore the expected upstream file: $TARGET"

  echo "✓ reverted: $TARGET"
  for b in "$TARGET".orig "$TARGET".orig-*; do
    [ -e "$b" ] && echo "  backup left in place: $b"
  done
  return 0
}

do_status() {
  preflight
  case "$(target_state)" in
    patched)
      echo "applied     $TARGET"
      echo "allow-list  ${HERMES_ACP_ALLOWED_MCP_TOOLS:-<unset — denies everything, same as stock>}"
      ;;
    stock)
      echo "not applied $TARGET (stock Hermes: denies every permission request)"
      ;;
    unknown)
      echo "unknown     $TARGET does not match stock or patched — see --revert/apply for details" >&2
      exit 1
      ;;
  esac
}

case "${1:-}" in
  "")        do_apply ;;
  --revert)  do_revert ;;
  --status)  do_status ;;
  -h|--help) sed -n '2,${/^#/!q;p;}' "$0" ;;
  *)         die "unknown argument: $1" ;;
esac
