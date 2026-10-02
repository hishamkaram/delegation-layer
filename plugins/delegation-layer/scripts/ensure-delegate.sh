#!/usr/bin/env sh
set -eu

fail() {
  printf 'delegation-layer plugin: %s\n' "$1" >&2
  exit 1
}

plugin_root="${PLUGIN_ROOT:-${CLAUDE_PLUGIN_ROOT:-}}"
[ -n "$plugin_root" ] || fail 'the plugin root environment variable is missing'

installer="$plugin_root/scripts/install-cli.sh"
[ -x "$installer" ] || fail "the bundled CLI installer is missing: $installer"

resolve_command_path() {
  path="$1"
  case "$path" in
    /*) ;;
    *) path="$(cd -P "$(dirname "$path")" 2>/dev/null && pwd)/$(basename "$path")" || return 1 ;;
  esac
  while [ -L "$path" ]; do
    link="$(readlink "$path")" || return 1
    case "$link" in
      /*) path="$link" ;;
      *) path="$(dirname "$path")/$link" ;;
    esac
  done
  path="$(cd -P "$(dirname "$path")" 2>/dev/null && pwd)/$(basename "$path")" || return 1
  printf '%s\n' "$path"
}

valid_delegate() {
  candidate="$1"
  [ -f "$candidate" ] && [ -x "$candidate" ] || return 1
  resolved_candidate="$(resolve_command_path "$candidate")" || return 1
  resolved_bin_dir="$(dirname "$resolved_candidate")"
  supervisor_dir="$resolved_bin_dir"
  if [ ! -x "$supervisor_dir/pueue" ] || [ ! -x "$supervisor_dir/pueued" ]; then
    supervisor_dir="$resolved_bin_dir/../libexec"
  fi
  delegate_run="$resolved_bin_dir/delegate-run"
  pueue="$supervisor_dir/pueue"
  pueued="$supervisor_dir/pueued"
  [ -f "$delegate_run" ] && [ -x "$delegate_run" ] || return 1
  [ -f "$pueue" ] && [ -x "$pueue" ] || return 1
  [ -f "$pueued" ] && [ -x "$pueued" ] || return 1

  delegate_version="$("$candidate" --version 2>/dev/null)" || return 1
  case "$delegate_version" in
    delegate\ *) ;;
    *) return 1 ;;
  esac
  pueue_version="$("$pueue" --version 2>/dev/null)" || return 1
  pueued_version="$("$pueued" --version 2>/dev/null)" || return 1
  case "$pueue_version" in
    pueue\ *) ;;
    *) return 1 ;;
  esac
  case "$pueued_version" in
    pueued\ *) ;;
    *) return 1 ;;
  esac
  pueue_release="$(printf '%s\n' "$pueue_version" | awk 'NF { print $NF; exit }')"
  pueued_release="$(printf '%s\n' "$pueued_version" | awk 'NF { print $NF; exit }')"
  [ -n "$pueue_release" ] && [ "$pueue_release" = "$pueued_release" ]
}

path_points_to() {
  expected="$1"
  actual="$(command -v delegate 2>/dev/null || true)"
  [ -n "$actual" ] || return 1
  actual="$(resolve_command_path "$actual")" || return 1
  expected="$(resolve_command_path "$expected")" || return 1
  [ "$actual" = "$expected" ]
}

path_delegate="$(command -v delegate 2>/dev/null || true)"
if [ -n "$path_delegate" ] && valid_delegate "$path_delegate"; then
  exit 0
fi

existing_install_dir="${DELEGATION_LAYER_INSTALL_DIR:-$HOME/.local/bin}"
existing_delegate="$existing_install_dir/delegate"
if valid_delegate "$existing_delegate"; then
  case ":${PATH:-}:" in
    *:"$existing_install_dir":*)
      path_points_to "$existing_delegate" && exit 0
      shadowed_delegate="$(command -v delegate 2>/dev/null || true)"
      fail "delegate is installed at $existing_install_dir but PATH resolves delegate to ${shadowed_delegate:-an unavailable command}; put $existing_install_dir before the shadowing entry and start a new harness session"
      ;;
    *)
      fail "delegate is installed at $existing_install_dir but that directory is not on PATH; add it to PATH and start a new harness session"
      ;;
  esac
fi

install_dir="$existing_install_dir"
case "$install_dir" in
  /*) ;;
  *) fail "the CLI install directory must be an absolute path: $install_dir" ;;
esac

if [ -e "$install_dir/delegate" ] || [ -L "$install_dir/delegate" ]; then
  fail "refusing to overwrite an existing delegate at $install_dir/delegate; put a clean directory first in PATH or set DELEGATION_LAYER_INSTALL_DIR to a dedicated directory"
fi

DELEGATION_LAYER_INSTALL_DIR="$install_dir" sh "$installer" ||
  fail 'the checksum-verified CLI installation failed'

installed_delegate="$install_dir/delegate"
valid_delegate "$installed_delegate" ||
  fail "the installer completed but delegate is not usable at $installed_delegate"

if path_points_to "$installed_delegate"; then
  printf 'Delegation Layer CLI is ready at %s\n' "$installed_delegate"
  exit 0
fi

path_delegate="$(command -v delegate 2>/dev/null || true)"
if [ -n "$path_delegate" ]; then
  fail "delegate was installed at $install_dir but PATH resolves delegate to $path_delegate; put $install_dir before the shadowing entry and start a new harness session"
fi
fail "delegate was installed at $install_dir but that directory is not on PATH; add it to PATH and start a new harness session"
