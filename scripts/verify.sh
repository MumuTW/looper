#!/usr/bin/env bash
# Local mirror of CI's blocking gates — run this before you push and CI won't
# surprise you. Covers .github/workflows/ci.yml's `verify` and `race` jobs:
#   optional gofmt -w → dashboard (pnpm install/test/build + artifact checks)
#   → Hermes/Devin helper tests → gofmt -l → go vet → production-only staticcheck
#   → frozen /api/v1 contract artifacts → go test
#   → go test -race (focused)
#   → go build (with release ldflags)
#
# The remaining ci.yml jobs (contract/invariant smoke and the conditional E2E
# jobs) run `-run`-filtered subsets of ./internal/e2e, which plain `go test
# ./...` above already covers in full — so there is nothing left for them to
# catch that a green run here would have missed.
#
# Usage:
#   scripts/verify.sh                 # check everything (fails like CI would)
#   scripts/verify.sh --fix           # gofmt -w first, then run the gates
#   scripts/verify.sh --install-hooks # point git at .githooks (one-time per clone)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

FIX=0
for arg in "$@"; do
  case "$arg" in
    --fix) FIX=1 ;;
    --install-hooks)
      git config core.hooksPath .githooks
      chmod +x .githooks/* 2>/dev/null || true
      echo "✓ git hooks enabled (core.hooksPath=.githooks) — pre-commit now auto-gofmts"
      exit 0 ;;
    -h|--help)
      # Print the leading comment block, however long it grows.
      sed -n '2,${/^#/!q;p;}' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

step() { printf '\n\033[1m▸ %s\033[0m\n' "$1"; }

if [ "$FIX" -eq 1 ]; then
  step "gofmt --fix"
  gofmt -w .
  echo "  gofmt -w applied"
fi

for tool in node pnpm python3; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "missing required dashboard tool: $tool (see CONTRIBUTING.md prerequisites)" >&2
    exit 1
  fi
done

step "dashboard (pnpm install/test/build + artifact checks)"
(
  cd web/dashboard
  pnpm install --frozen-lockfile
  pnpm test
  rm -rf ../../internal/dashboard/assets/assets
  rm -f ../../internal/dashboard/assets/.production ../../internal/dashboard/assets/index.html
  pnpm run build
  test -f ../../internal/dashboard/assets/.production
  test -f ../../internal/dashboard/assets/index.html
)
echo "  clean"

step "Hermes/Devin helper tests"
python3 -m unittest discover -s tools/hermes-devin -p 'test_*.py'

# Mirrors ci.yml's `semantic-prefix` job. AGENTS.md requires a semantic prefix
# on every commit subject and PR title; Mergify merges (not squashes) into
# main, so every non-merge commit subject on the branch lands verbatim. Merge
# commits are skipped (their subjects are not authored semantic messages).
step "semantic prefix (commit subjects vs origin/main)"
scripts/check-semantic-prefix.test.sh
if git rev-parse --verify origin/main >/dev/null 2>&1; then
  git log --no-merges --format=%s origin/main..HEAD \
    | scripts/check-semantic-prefix.sh --stdin
else
  echo "  origin/main not found; skipping range check (no remote configured)"
fi

step "gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  printf '  these files need gofmt (run: scripts/verify.sh --fix):\n%s\n' "$unformatted" >&2
  exit 1
fi
echo "  clean"

step "go vet ./..."
go vet ./...

step "staticcheck (production-only unreachable code)"
go run honnef.co/go/tools/cmd/staticcheck@v0.6.1 \
  -tests=false \
  -checks='U1000,SA1006,SA4004,SA4006' \
  ./...

# Same gate as ci.yml's "Check frozen HTTP contract artifacts are current", and
# it runs BEFORE `go test ./...` for the same reason: the frozen-artifact table
# tests read these files, so a stale artifact fails them first and this
# actionable message would never print.
#
# Unlike CI, this runs against your working tree, which may legitimately hold
# regenerated-but-uncommitted artifacts. The question here is therefore "did
# regeneration change anything?", not "does the tree match HEAD?" — comparing
# against HEAD would fail a developer whose artifacts are already current.
step "frozen /api/v1 contract artifacts"
contract_artifact_digest() {
  find internal/api/testdata/contracts -type f | LC_ALL=C sort | while IFS= read -r file; do
    printf '%s  %s\n' "$(git hash-object -- "$file")" "$file"
  done
}
contracts_before="$(contract_artifact_digest)"
# go generate runs the regenerator as a `go test` invocation; its stdout carries
# the route and response detail that explains a capture failure, so hold it and
# replay it instead of discarding it.
if ! contracts_log="$(go generate ./internal/api/... 2>&1)"; then
  printf '%s\n' "$contracts_log" >&2
  printf '\n  regenerating the frozen /api/v1 compat artifacts failed; see the output above\n\n' >&2
  exit 1
fi
if [ "$contracts_before" != "$(contract_artifact_digest)" ]; then
  printf '  the frozen /api/v1 compat artifacts were stale; regeneration has just rewritten them:\n\n    git diff -- internal/api/testdata/contracts/\n\n  review that diff and include it in this commit.\n\n' >&2
  exit 1
fi
echo "  current"

step "go test ./..."
go test ./...

# Same focused package set as ci.yml's `race` job — both read this file, so the
# two cannot drift. -race is the one gate `go test ./...` structurally cannot
# substitute for: without it there is no race detector at all.
step "go test -race (focused)"
race_packages=()
while IFS= read -r line; do
  [ -z "$line" ] && continue
  case "$line" in \#*) continue ;; esac
  race_packages+=("$line")
done < scripts/race-packages.txt
if [ "${#race_packages[@]}" -eq 0 ]; then
  echo "  scripts/race-packages.txt lists no packages" >&2
  exit 1
fi
go test -race -count=1 "${race_packages[@]}"

step "go build (release ldflags)"
LOOPER_BUILD_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
LOOPER_BUILD_GIT_SHA="$(git rev-parse HEAD)" \
  go build -ldflags "$(go run ./tools/go-build-flags)" ./...

printf '\n\033[32m✓ passed — matches CI'"'"'s verify + race jobs\033[0m\n'
