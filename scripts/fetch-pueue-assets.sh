#!/usr/bin/env bash
set -euo pipefail

pueue_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
exec python3 "${pueue_script_dir}/fetch_pueue_assets.py" "$@"
