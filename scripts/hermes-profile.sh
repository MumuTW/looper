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

# Strict mode only when executed. When sourced, `set -e` would leak into the
# caller's interactive shell and kill it on the next failing command.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  set -euo pipefail
fi

HERMES_ROOT="${HERMES_ROOT:-$HOME/.hermes}"
LOOPER_HERMES_PROFILE="${LOOPER_HERMES_PROFILE:-looper}"
LOOPER_HERMES_HOME="$HERMES_ROOT/profiles/$LOOPER_HERMES_PROFILE"
LOOPER_DEVIN_MODEL="${LOOPER_DEVIN_MODEL:-glm-5-2}"

bootstrap() {
  if [ ! -d "$LOOPER_HERMES_HOME" ]; then
    hermes profile create "$LOOPER_HERMES_PROFILE" --no-alias \
      --description "Looper repo profile: Devin ACP backend, repo-scoped memory"
  fi

  cat > "$LOOPER_HERMES_HOME/config.yaml" <<YAML
model:
  default: copilot-acp
  provider: copilot-acp
agent:
  max_turns: 60
memory:
  memory_enabled: true
  user_profile_enabled: true
YAML

  cat > "$LOOPER_HERMES_HOME/.env" <<ENV
# Devin CLI as the ACP model backend. Hermes's copilot-acp provider speaks
# generic ACP v1; these two vars repoint it at \`devin acp\` with no patch.
HERMES_COPILOT_ACP_COMMAND=devin
HERMES_COPILOT_ACP_ARGS=acp --model $LOOPER_DEVIN_MODEL
ENV

  echo "Bootstrapped Hermes profile '$LOOPER_HERMES_PROFILE' at $LOOPER_HERMES_HOME"
  echo "Model backend: devin acp --model $LOOPER_DEVIN_MODEL"
  echo "Memory lives in $LOOPER_HERMES_HOME/memories/ (separate from your default profile)"
}

case "${1:-}" in
  --bootstrap)
    bootstrap
    exit 0
    ;;
  --print)
    echo "$LOOPER_HERMES_HOME"
    exit 0
    ;;
  "")
    ;;
  *)
    echo "unknown argument: $1" >&2
    exit 2
    ;;
esac

if [ ! -d "$LOOPER_HERMES_HOME" ]; then
  echo "Hermes profile '$LOOPER_HERMES_PROFILE' does not exist." >&2
  echo "Run: scripts/hermes-profile.sh --bootstrap" >&2
  return 1 2>/dev/null || exit 1
fi

export HERMES_HOME="$LOOPER_HERMES_HOME"
