#!/bin/sh
set -eu

status=0

say() {
  printf '%s\n' "$1"
}

check_cmd() {
  name="$1"
  if command -v "$name" >/dev/null 2>&1; then
    say "ok: $name -> $(command -v "$name")"
  else
    say "missing: $name"
    status=1
  fi
}

say "Looper environment check (read-only)"

home="${HOME:-}"
if [ -z "$home" ]; then
  say "error: HOME is not set"
  exit 1
fi

check_cmd git
check_cmd gh

if command -v gh >/dev/null 2>&1; then
  if gh auth status >/dev/null 2>&1; then
    say "ok: gh auth status works"
  else
    say "warn: gh found but auth status failed"
  fi
fi

if command -v osascript >/dev/null 2>&1; then
  say "ok: osascript -> $(command -v osascript)"
else
  say "warn: osascript not found (required only when notifications.osascript.enabled is true)"
fi

if command -v looper >/dev/null 2>&1; then
  say "ok: looper -> $(command -v looper)"
  if looper --version >/dev/null 2>&1; then
    say "ok: looper --version works"
  else
    say "warn: looper found but --version failed"
  fi
else
  say "warn: looper not found on PATH"
fi

# Match Go DiscoverDefaultConfigPath: LOOPER_CONFIG, else first existing among
# config.toml → config.yaml → config.yml → config.json; canonical default is toml.
if [ -n "${LOOPER_CONFIG:-}" ]; then
  config_path="$LOOPER_CONFIG"
else
  config_path=""
  for name in config.toml config.yaml config.yml config.json; do
    candidate="$home/.looper/$name"
    if [ -f "$candidate" ]; then
      config_path="$candidate"
      break
    fi
  done
  if [ -z "$config_path" ]; then
    config_path="$home/.looper/config.toml"
  fi
fi
if [ -f "$config_path" ]; then
  say "ok: config exists at $config_path"
else
  say "warn: config not found at $config_path"
fi

runtime_dir="$home/.looper"
if [ -d "$runtime_dir" ]; then
  if [ -w "$runtime_dir" ]; then
    say "ok: runtime directory writable at $runtime_dir"
  else
    say "error: runtime directory is not writable at $runtime_dir"
    status=1
  fi
else
  say "warn: runtime directory does not exist at $runtime_dir (not creating it)"
fi

exit "$status"
