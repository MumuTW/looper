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

# Match Go DiscoverDefaultConfigPath: LOOPER_CONFIG, else inspect every
# supported default and reject ambiguity (except canonical TOML + legacy JSON).
if [ -n "${LOOPER_CONFIG:-}" ]; then
  config_path="$LOOPER_CONFIG"
else
  config_path=""
  found_count=0
  found_paths=""
  found_toml=0
  found_json=0
  for name in config.toml config.yaml config.yml config.json; do
    candidate="$home/.looper/$name"
    if [ -f "$candidate" ]; then
      config_path="$candidate"
      found_count=$((found_count + 1))
      found_paths="${found_paths}${found_paths:+, }$candidate"
      [ "$name" = "config.toml" ] && found_toml=1
      [ "$name" = "config.json" ] && found_json=1
    fi
  done
  if [ "$found_count" -gt 1 ]; then
    if [ "$found_count" -eq 2 ] && [ "$found_toml" -eq 1 ] && [ "$found_json" -eq 1 ]; then
      config_path="$home/.looper/config.toml"
    else
      say "error: multiple default config files found: $found_paths; keep only one"
      status=1
      config_path=""
    fi
  elif [ "$found_count" -eq 0 ]; then
    config_path="$home/.looper/config.toml"
  fi
fi
if [ -z "$config_path" ]; then
  : # Ambiguity was reported above; do not claim a usable config.
elif [ -f "$config_path" ]; then
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
