#!/bin/sh
set -eu
umask 077

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
provider=${1:?usage: acceptance_native.sh PROVIDER [options]}
shift

if [ "$provider" = opencode:run ] && [ -n "${OPENCODE_LIVE_MODEL:-}" ]; then
    model_supplied=false
    for option in "$@"; do
        case "$option" in
            --model|--model=*) model_supplied=true ;;
        esac
    done
    if [ "$model_supplied" = false ]; then
        set -- --model "$OPENCODE_LIVE_MODEL" "$@"
    fi
fi

exec python3 "$repo_root/scripts/acceptance_native.py" \
    --provider "$provider" \
    --delegate "$repo_root/bin/delegate" \
    --runner "$repo_root/bin/delegate-run" "$@"
