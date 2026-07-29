#!/usr/bin/env bash
# Local mirror of CI's `verify` job — run this before you push and CI won't
# surprise you. Same gates, same order as .github/workflows/ci.yml:
#   optional gofmt -w → dashboard (pnpm install/test/build + artifact checks)
#   → gofmt -l  →  go vet  →  go test  →  go build (with release ldflags)
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
      sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

step() { printf '\n\033[1m▸ %s\033[0m\n' "$1"; }

if [ "$FIX" -eq 1 ]; then
  step "gofmt --fix"
  gofmt -w .
  echo "  gofmt -w applied"
fi

for tool in node pnpm; do
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

step "gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  printf '  these files need gofmt (run: scripts/verify.sh --fix):\n%s\n' "$unformatted" >&2
  exit 1
fi
echo "  clean"

step "go vet ./..."
go vet ./...

step "go test ./..."
go test ./...

step "go build (release ldflags)"
LOOPER_BUILD_TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
LOOPER_BUILD_GIT_SHA="$(git rev-parse HEAD)" \
  go build -ldflags "$(go run ./tools/go-build-flags)" ./...

printf '\n\033[32m✓ verify passed — matches CI\033[0m\n'
