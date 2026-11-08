#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Preserve the existing finite primary retry budget. The API route is tried once.
TOOL_DOWNLOAD_OPTIONS=(--retry 3 --retry-all-errors --retry-delay 2 --retry-max-time 180 --connect-timeout 15 --max-time 120)

download_tool_archive() {
    local url="$1" destination="$2" max_bytes="$3" http_status status downloaded_bytes
    local -r maximum_bytes=9223372036854775807
    local LC_ALL=C
    if [[ "${max_bytes}" =~ ^[+]?[0123456789]+$ ]]; then
        max_bytes="${max_bytes#+}"
        max_bytes="${max_bytes#"${max_bytes%%[!0]*}"}"
    else
        max_bytes=""
    fi
    # Equal-length canonical decimals use lexical order without integer overflow.
    if [[ -z "${max_bytes}" || "${#max_bytes}" -gt 19 ||
          ( "${#max_bytes}" -eq 19 && "decimal:${max_bytes}" > "decimal:${maximum_bytes}" ) ]]; then
        echo "ERROR: MAX_BYTES must be a positive decimal integer no greater than ${maximum_bytes}." >&2
        return 2
    fi
    if http_status="$(curl "${TOOL_DOWNLOAD_OPTIONS[@]}" -sSL -f --max-filesize "${max_bytes}" --write-out '%{http_code}' -o "${destination}" "${url}")"; then
        downloaded_bytes="$(wc -c < "${destination}")"
        if [[ "${downloaded_bytes}" -gt "${max_bytes}" ]]; then
            echo "ERROR: downloaded archive exceeds MAX_BYTES (${max_bytes})." >&2
            return 63
        fi
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
