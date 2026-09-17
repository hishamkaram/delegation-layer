#!/bin/sh
set -eu
umask 077

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
provider=${1:?usage: acceptance_native.sh PROVIDER [options]}
shift

exec python3 "$repo_root/scripts/acceptance_native.py" \
    --provider "$provider" \
    --delegate "$repo_root/bin/delegate" \
    --runner "$repo_root/bin/delegate-run" "$@"
