#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Preserve the existing finite primary retry budget. The API route is tried once.
TOOL_DOWNLOAD_OPTIONS=(--retry 3 --retry-all-errors --retry-delay 2 --retry-max-time 180 --connect-timeout 15 --max-time 120)

download_tool_archive() {
    local url="$1" destination="$2" max_bytes="$3" http_status status
    if http_status="$(curl "${TOOL_DOWNLOAD_OPTIONS[@]}" -sSL -f --write-out '%{http_code}' -o "${destination}" "${url}")"; then
        return 0
    else
        status=$?
    fi
    # Only transport interruption or a server error may try the same asset's
    # API route. Certificate, permanent HTTP and local write errors stay errors.
    if [[ "${status}" =~ ^(7|18|28|52|55|56)$ || ( "${status}" -eq 22 && "${http_status}" =~ ^5[0-9][0-9]$ ) ]]; then
        echo "Release download failed (curl ${status}, HTTP ${http_status}); trying the exact pinned asset through GitHub's API." >&2
        if python3 "${SCRIPT_DIR}/github_release_asset.py" --url "${url}" --max-bytes "${max_bytes}" >"${destination}"; then
            return 0
        fi
        echo "ERROR: API fallback failed after release download failure (curl ${status}, HTTP ${http_status})." >&2
        return 1
    fi
    return "${status}"
}

if [[ "$#" -ne 3 ]]; then
    echo "Usage: download-tool-archive.sh URL OUTPUT MAX_BYTES" >&2
    exit 2
fi
download_tool_archive "$1" "$2" "$3"
