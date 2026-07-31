#!/usr/bin/env bash
# Validate commit/PR subjects against the AGENTS.md semantic-prefix rule.
#
# AGENTS.md requires every commit subject and PR title to start with one of:
#   feat, fix, docs, test, refactor, chore, ci
# followed by an optional scope in parentheses, an optional `!`, then `: `.
#
# This is a deterministic, regex-checkable rule. Enforcing it in CI (instead
# of via an LLM reviewer) gives instant, consistent feedback and frees review
# rounds for the design-guideline checks that genuinely need contextual
# judgment. See MumuTW/looper#470.
#
# Usage:
#   scripts/check-semantic-prefix.sh <subject>          # validate one subject
#   printf 'subj1\nsubj2\n' | scripts/check-semantic-prefix.sh --stdin
#
# Exit status is non-zero if any subject fails. The allowed prefixes are
# quoted in the failure message so the fix is obvious without opening AGENTS.md.
set -euo pipefail

pattern='^(feat|fix|docs|test|refactor|chore|ci)(\(.+\))?!?: '

fail_message() {
  cat >&2 <<'EOF'

AGENTS.md requires a semantic prefix on every commit subject and PR title:

  ^(feat|fix|docs|test|refactor|chore|ci)(\(.+\))?!?: <description>

Allowed prefixes: feat, fix, docs, test, refactor, chore, ci
Examples:
  feat: add semantic-prefix CI check
  fix(api): handle nil response
  refactor!: drop the legacy ledger field
EOF
}

# validate_one <subject> -> 0 ok, 1 bad. Prints the offending subject on failure.
validate_one() {
  local subject="$1"
  if printf '%s' "$subject" | grep -Eq "$pattern"; then
    return 0
  fi
  printf '✗ not semantic: %s\n' "$subject" >&2
  return 1
}

main() {
  local rc=0
  if [ "${1:-}" = "--stdin" ]; then
    shift
    while IFS= read -r line; do
      [ -z "$line" ] && continue
      validate_one "$line" || rc=1
    done
  elif [ $# -ge 1 ]; then
    validate_one "$1" || rc=1
  else
    echo "usage: $0 <subject> | --stdin (one subject per line)" >&2
    return 2
  fi
  if [ $rc -ne 0 ]; then
    fail_message
  fi
  return $rc
}

main "$@"
