#!/bin/sh
set -u
umask 077

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
status=0
for provider in pi:json opencode:run; do
    set +e
    python3 "$repo_root/scripts/acceptance_native.py" \
        --provider "$provider" \
        --delegate "$repo_root/bin/delegate" \
        --runner "$repo_root/bin/delegate-run"
    result=$?
    set -e
    case "$result" in
        0|2) ;;
        *) status=1 ;;
    esac
done
exit "$status"
