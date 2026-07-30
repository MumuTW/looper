#!/usr/bin/env bash
# Reject binary files that nobody deliberately added.
#
# A 3.5 MB compiled arm64 executable (`fake-agent`) reached main and became the
# largest file in the repository. `.gitignore` could not have caught it: it names
# specific paths, and a build artifact can be named anything. So the rule here is
# the inverse — binaries are refused unless their extension is on the allowlist
# below, which is short because every legitimate binary in this repo is an image
# or an icon.
#
# Size is deliberately not a threshold. A committed executable is wrong at any
# size, and a threshold only moves the argument to what the number should be.
#
# Usage:
#   scripts/check-tracked-binaries.sh            # every tracked file
#   scripts/check-tracked-binaries.sh --staged   # only what is staged (pre-commit)
#
# Kept bash-3.2 compatible (macOS default bash) — no mapfile/readarray.
set -euo pipefail

allowed() {
  case "${1##*/}" in
  *.png | *.ico | *.jpg | *.jpeg | *.gif | *.webp | *.woff | *.woff2) return 0 ;;
  *) return 1 ;;
  esac
}

if [ "${1:-}" = "--staged" ]; then
  list() { git diff --cached --name-only -z --diff-filter=ACM; }
else
  list() { git ls-files -z; }
fi

offenders=""
while IFS= read -r -d '' f; do
  [ -f "$f" ] || continue
  # An empty file has no NUL bytes to find but also no content to be binary.
  [ -s "$f" ] || continue
  allowed "$f" && continue
  # grep -I reports "binary file matches" semantics: non-zero exit for binaries.
  # Portable across GNU and BSD grep, unlike file(1) output parsing.
  if ! LC_ALL=C grep -qI . "$f" 2>/dev/null; then
    offenders="${offenders}  ${f} ($(wc -c <"$f" | tr -d ' ') bytes)
"
  fi
done < <(list)

[ -z "$offenders" ] && exit 0

cat >&2 <<EOF
Refusing binary files that are not on the allowlist:

${offenders}
Build output belongs in dist/ and stays untracked (see AGENTS.md). If one of
these is a deliberate asset, add its extension to the allowlist in
scripts/check-tracked-binaries.sh and say why in the pull request.
EOF
exit 1
