#!/bin/sh

set -eu

log() {
  printf '%s\n' "$*"
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

uninstall_yes_enabled() {
  case "${LOOPER_UNINSTALL_YES:-}" in
    1|true|TRUE|yes|YES) return 0 ;;
    ""|0|false|FALSE|no|NO) return 1 ;;
    *) fail "LOOPER_UNINSTALL_YES must be 1/true/yes or 0/false/no" ;;
  esac
}

confirm() {
  prompt="$1"
  if uninstall_yes_enabled; then
    printf '%s [approved by LOOPER_UNINSTALL_YES]\n' "$prompt" >&2
    return 0
  fi
  if [ ! -t 0 ]; then
    return 1
  fi
  printf '%s [y/N] ' "$prompt" >&2
  read -r answer || return 1
  case "$answer" in
    y|Y|yes|YES) return 0 ;;
    *) return 1 ;;
  esac
}

remove_if_exists() {
  path="$1"
  if [ -e "$path" ] || [ -L "$path" ]; then
    rm -rf "$path"
    log "Removed $path"
  fi
}

remove_installer_profile_stanza() {
  profile="$1"
  if [ ! -f "$profile" ]; then
    return
  fi

  tmp_profile="$(mktemp "${TMPDIR:-/tmp}/looper-profile.XXXXXX")"
  awk '
    BEGIN {
      marker = "# Added by looper installer"
      target = "export PATH=\"$HOME/.local/bin:$PATH\""
    }
    {
      line = $0
      if (pending_marker) {
        if (line == target) {
          if (have_previous && previous != "") print previous
          have_previous = 0
          pending_marker = 0
          next
        }
        if (have_previous) print previous
        print marker
        have_previous = 0
        pending_marker = 0
      }
      if (line == marker) {
        pending_marker = 1
        next
      }
      if (have_previous) print previous
      previous = line
      have_previous = 1
    }
    END {
      if (pending_marker) {
        if (have_previous) print previous
        print marker
        have_previous = 0
      }
      if (have_previous) print previous
    }
  ' "$profile" >"$tmp_profile"

  if cmp -s "$profile" "$tmp_profile"; then
    rm -f "$tmp_profile"
    return
  fi
  # Overwrite the existing file rather than replacing it so user ownership,
  # permissions, and any symlink target remain unchanged.
  if ! cat "$tmp_profile" >"$profile"; then
    rm -f "$tmp_profile"
    fail "could not update shell profile: $profile"
  fi
  rm -f "$tmp_profile"
  log "Removed Looper PATH stanza from $profile"
}

path_exists() {
  [ -e "$1" ] || [ -L "$1" ]
}

list_if_exists() {
  if path_exists "$1"; then
    log "  - $1"
  fi
}

has_optional_data() {
  for path in \
    "$looper_home/config.toml" \
    "$looper_home/config.json" \
    "$looper_home/config.yaml" \
    "$looper_home/config.yml" \
    "$looper_home/looper.sqlite" \
    "$looper_home/looper.sqlite-wal" \
    "$looper_home/looper.sqlite-shm" \
    "$looper_home/backups" \
    "$looper_home/logs" \
    "$looper_home/worktrees"
  do
    if path_exists "$path"; then
      return 0
    fi
  done
  return 1
}

list_optional_data() {
  list_if_exists "$looper_home/config.toml"
  list_if_exists "$looper_home/config.json"
  list_if_exists "$looper_home/config.yaml"
  list_if_exists "$looper_home/config.yml"
  list_if_exists "$looper_home/looper.sqlite"
  list_if_exists "$looper_home/looper.sqlite-wal"
  list_if_exists "$looper_home/looper.sqlite-shm"
  list_if_exists "$looper_home/backups"
  list_if_exists "$looper_home/logs"
  list_if_exists "$looper_home/worktrees"
}

remove_optional_data() {
  remove_if_exists "$looper_home/config.toml"
  remove_if_exists "$looper_home/config.json"
  remove_if_exists "$looper_home/config.yaml"
  remove_if_exists "$looper_home/config.yml"
  remove_if_exists "$looper_home/looper.sqlite"
  remove_if_exists "$looper_home/looper.sqlite-wal"
  remove_if_exists "$looper_home/looper.sqlite-shm"
  remove_if_exists "$looper_home/backups"
  remove_if_exists "$looper_home/logs"
  remove_if_exists "$looper_home/worktrees"
}

in_path_dir() {
  candidate="$1"
  old_ifs=$IFS
  IFS=:
  for entry in $PATH; do
    [ "$entry" = "$candidate" ] && IFS=$old_ifs && return 0
  done
  IFS=$old_ifs
  return 1
}

is_installer_owned_cli_path() {
  path="$1"
  dir="${path%/*}"
  case "$path" in
    "$HOME/.local/bin/looper"|"$HOME/.looper/bin/looper") return 0 ;;
    "$HOME"/go/bin/looper|"$HOME"/*/go/bin/looper) return 1 ;;
    /opt/homebrew/*|/usr/local/Homebrew/*) return 1 ;;
    "$HOME"/*/looper)
      if in_path_dir "$dir"; then
        return 0
      fi
      return 1
      ;;
    *) return 1 ;;
  esac
}

cli_path="${LOOPER_INSTALL_PATH:-}"
explicit_cli_path=0
if [ -n "$cli_path" ]; then
  explicit_cli_path=1
elif command -v looper >/dev/null 2>&1; then
  cli_path="$(command -v looper)"
fi

looper_home="${LOOPER_HOME:-$HOME/.looper}"
if [ -d "$looper_home" ]; then
  looper_home="$(CDPATH= cd -P "$looper_home" 2>/dev/null && pwd -P)" || fail "could not resolve LOOPER_HOME: $looper_home"
fi
case "$looper_home" in
  /*) ;;
  *) fail "LOOPER_HOME must be an absolute path for uninstall: $looper_home" ;;
esac
case "$looper_home" in
  /|"$HOME") fail "refusing unsafe LOOPER_HOME for uninstall: $looper_home" ;;
esac

if [ -n "$cli_path" ]; then
  if is_installer_owned_cli_path "$cli_path"; then
    remove_if_exists "$cli_path"
  elif [ "$explicit_cli_path" -eq 1 ] && confirm "Remove CLI binary at $cli_path? This path is not recognized as installer-owned."; then
    remove_if_exists "$cli_path"
  else
    log "Skipped CLI binary at $cli_path (not recognized as installer-owned; set LOOPER_INSTALL_PATH and confirm to remove)"
  fi
fi

remove_if_exists "$looper_home/bin/looperd"
remove_if_exists "$looper_home/bin/looperd.prev"
remove_if_exists "$looper_home/state"
remove_if_exists "$looper_home/run/upgrade.lock"

remove_installer_profile_stanza "$HOME/.zprofile"
remove_installer_profile_stanza "$HOME/.bash_profile"
remove_installer_profile_stanza "$HOME/.profile"

if has_optional_data; then
  log "The following Looper data exists and will be removed only with your approval:"
  list_optional_data
  if confirm "Remove exactly the listed Looper data?"; then
    remove_optional_data
  else
    log "Kept the listed Looper data"
  fi
fi

log "Looper uninstall complete"
