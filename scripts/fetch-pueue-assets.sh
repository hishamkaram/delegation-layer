#!/usr/bin/env bash
set -euo pipefail

pueue_script_dir="$(dirname "${BASH_SOURCE[0]}")"
pueue_script_dir="$(cd "${pueue_script_dir}" && pwd -P)"
exec python3 "${pueue_script_dir}/fetch_pueue_assets.py" "$@"
