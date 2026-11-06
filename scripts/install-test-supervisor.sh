#!/usr/bin/env bash
set -euo pipefail

# Installation only: the acceptance owner selects configuration and starts work.
supervisor_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
exec "${supervisor_script_dir}/fetch-pueue-assets.sh" --host "${supervisor_script_dir}/../bin/test-supervisor"
