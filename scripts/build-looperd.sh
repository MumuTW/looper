#!/usr/bin/env bash
# Build a distributable looperd binary with the production dashboard embedded.
# Plain `go build` intentionally remains the fast Go-only development path and
# may embed the fallback page when no prior dashboard build exists.

set -euo pipefail

script_dir="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(CDPATH='' cd -- "$script_dir/.." && pwd)"
cd "$repo_root"

assets_ready=0
output_path="dist/looperd"

usage() {
  sed -n '2,5p' "$0" | sed 's/^# \{0,1\}//'
  printf '\nUsage: scripts/build-looperd.sh [--assets-ready] [--output <path>]\n'
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --assets-ready)
      assets_ready=1
      shift
      ;;
    --output)
      [ "$#" -ge 2 ] || { printf '%s\n' 'error: --output requires a path' >&2; exit 2; }
      output_path="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'error: unknown argument: %s\n' "$1" >&2
      exit 2
      ;;
  esac
done

if [ "$assets_ready" -eq 0 ]; then
  for required_tool in node pnpm; do
    if ! command -v "$required_tool" >/dev/null 2>&1; then
      printf 'error: %s is required to build the embedded dashboard\n' "$required_tool" >&2
      exit 1
    fi
  done
  (
    cd web/dashboard
    pnpm install --frozen-lockfile
    rm -rf ../../internal/dashboard/assets/assets
    rm -f ../../internal/dashboard/assets/.production ../../internal/dashboard/assets/index.html
    pnpm run build
  )
fi

for required_asset in internal/dashboard/assets/.production internal/dashboard/assets/index.html; do
  if [ ! -f "$required_asset" ]; then
    printf 'error: required dashboard asset is missing: %s\n' "$required_asset" >&2
    exit 1
  fi
done
if ! find internal/dashboard/assets/assets -type f -name '*.js' -print -quit | grep -q .; then
  printf '%s\n' 'error: dashboard build produced no JavaScript bundle' >&2
  exit 1
fi

mkdir -p "$(dirname "$output_path")"
export LOOPER_BUILD_GIT_SHA="${LOOPER_BUILD_GIT_SHA:-$(git rev-parse HEAD)}"
export LOOPER_BUILD_TIMESTAMP="${LOOPER_BUILD_TIMESTAMP:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
go build -trimpath -ldflags "$(go run ./tools/go-build-flags)" -o "$output_path" ./cmd/looperd

test -f "$output_path"
printf 'built looperd with production dashboard: %s\n' "$output_path"
