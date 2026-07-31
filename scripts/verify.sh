#!/usr/bin/env bash
# Local mirror of CI's blocking gates — run this before you push and CI won't
# surprise you. Covers .github/workflows/ci.yml's `verify` and `race` jobs:
#   optional gofmt -w → dashboard (pnpm install/test/build + artifact checks)
#   → Hermes/Devin helper tests → gofmt -l → go vet → production-only staticcheck → go test
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

step "go test ./..."
go test ./...

# Same gate as ci.yml's "Check frozen HTTP contract artifacts are current".
step "frozen /api/v1 contract artifacts"
go generate ./internal/api/... >/dev/null
if ! git diff --exit-code -- internal/api/testdata/contracts/ >/dev/null; then
  printf '  the frozen /api/v1 compat artifacts are stale; regenerate them in this commit:\n\n    go generate ./internal/api/...\n\n' >&2
  exit 1
fi
echo "  current"

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
