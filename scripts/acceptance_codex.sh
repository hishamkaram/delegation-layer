#!/bin/sh
set -eu
umask 077

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if ! pueue_path=$(command -v pueue) || ! pueued_path=$(command -v pueued); then
    echo 'BLOCKED: installed pueue and pueued are required' >&2
    exit 2
fi
evidence_root=$(python3 -c 'from pathlib import Path; import secrets; print(Path.home() / "Library/Application Support/delegation-layer-evidence" / ("codex-" + secrets.token_hex(12)))')
exec python3 "$repo_root/scripts/acceptance_codex.py" \
    --tools "$repo_root/bin/codex-acceptance" --pueue "$pueue_path" --pueued "$pueued_path" \
    --output "$evidence_root" "$@"
