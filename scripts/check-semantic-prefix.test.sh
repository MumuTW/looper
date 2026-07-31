#!/usr/bin/env bash
# Tests for scripts/check-semantic-prefix.sh.
#
# Covers the AGENTS.md semantic-prefix regex anchored in #470:
#   ^(feat|fix|docs|test|refactor|chore|ci)(\(.+\))?!?: <description>
#
# Run directly: scripts/check-semantic-prefix.test.sh
# Exit status is non-zero if any expectation fails.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
script="$here/check-semantic-prefix.sh"

pass=0
fail=0

expect_ok() {
  local subject="$1"
  if "$script" "$subject" >/dev/null 2>&1; then
    pass=$((pass + 1))
  else
    echo "FAIL (expected ok, got bad): $subject" >&2
    fail=$((fail + 1))
  fi
}

expect_bad() {
  local subject="$1"
  if ! "$script" "$subject" >/dev/null 2>&1; then
    pass=$((pass + 1))
  else
    echo "FAIL (expected bad, got ok): $subject" >&2
    fail=$((fail + 1))
  fi
}

# --- valid: every allowed prefix, with/without scope and `!` ---
expect_ok "feat: add semantic-prefix CI check"
expect_ok "fix: handle nil response"
expect_ok "docs: update AGENTS.md"
expect_ok "test: cover the prefix regex"
expect_ok "refactor: collapse duplicate else branches"
expect_ok "chore: bump deps"
expect_ok "ci: enforce semantic prefixes"
expect_ok "fix(api): handle nil response"
expect_ok "feat(very-long-scope): add thing"
expect_ok "refactor!: drop the legacy ledger field"
expect_ok "feat(scope)!: breaking change"
expect_ok "ci(workflows): add semantic-prefix job"

# --- invalid: missing prefix, wrong prefix, missing colon-space, casing ---
expect_bad "no prefix here"
expect_bad "feature: wrong prefix"
expect_bad "wip: not an allowed prefix"
expect_bad "Feat: capitalized prefix"
expect_bad "FIX: uppercase prefix"
expect_bad "feat:missing space after colon"
expect_bad "fix:no space"
expect_bad "feat(scope)missing colon"
expect_bad "feat(): empty scope is not .+"
# NOTE: the anchored regex only checks the prefix, so "feat: " (trailing space,
# empty description) is technically a match. The issue's regex has no
# description-length requirement, so we intentionally do NOT assert it as bad.

# --- stdin mode: mixed batch fails overall but reports only the bad ones ---
stdin_rc=0
printf 'feat: ok one\nbad no prefix\nfix: ok two\n' \
  | "$script" --stdin >/dev/null 2>&1 || stdin_rc=$?
if [ $stdin_rc -ne 0 ]; then
  pass=$((pass + 1))
else
  echo "FAIL (stdin batch with a bad subject should exit non-zero)" >&2
  fail=$((fail + 1))
fi

# --- stdin mode: all valid passes ---
if printf 'feat: a\nfix: b\nchore: c\n' | "$script" --stdin >/dev/null 2>&1; then
  pass=$((pass + 1))
else
  echo "FAIL (stdin batch of valid subjects should exit zero)" >&2
  fail=$((fail + 1))
fi

# --- usage error: no args exits non-zero ---
usage_rc=0
"$script" >/dev/null 2>&1 || usage_rc=$?
if [ $usage_rc -ne 0 ]; then
  pass=$((pass + 1))
else
  echo "FAIL (no args should exit non-zero)" >&2
  fail=$((fail + 1))
fi

echo "semantic-prefix tests: pass=$pass fail=$fail"
[ $fail -eq 0 ]
