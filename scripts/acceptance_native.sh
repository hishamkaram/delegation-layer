#!/bin/sh
set -eu
umask 077

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
provider=${1:?usage: acceptance_native.sh PROVIDER [options]}
shift

model=
case "$provider" in
    opencode:run) model=${OPENCODE_LIVE_MODEL:-} ;;
    ccr:print) model=${CCR_LIVE_MODEL:-} ;;
esac
if [ -n "$model" ]; then
    model_supplied=false
    for option in "$@"; do
        case "$option" in
            --model|--model=*) model_supplied=true ;;
        esac
    done
    if [ "$model_supplied" = false ]; then
        set -- --model "$model" "$@"
    fi
fi

exec python3 "$repo_root/scripts/acceptance_native.py" \
    --provider "$provider" \
    --delegate "$repo_root/bin/delegate" \
    --runner "$repo_root/bin/delegate-run" "$@"
