#!/usr/bin/env bash
set -euo pipefail

# Download only. The acceptance harness controls the supervisor configuration
# before invoking either executable; this script never contacts a queue.
supervisor_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
supervisor_bin_dir="${supervisor_script_dir}/../bin/test-supervisor"
supervisor_manifest="${supervisor_script_dir}/test-supervisor-sha256.txt"

case "$(uname -s)" in
    Darwin) supervisor_platform=apple-darwin ;;
    Linux) supervisor_platform=unknown-linux-musl ;;
    *) echo "Unsupported supervisor test platform" >&2; exit 1 ;;
esac
case "$(uname -m)" in
    arm64|aarch64) supervisor_arch=aarch64 ;;
    x86_64|amd64) supervisor_arch=x86_64 ;;
    *) echo "Unsupported supervisor test architecture" >&2; exit 1 ;;
esac

supervisor_sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

if [[ -L "${supervisor_bin_dir}" ]]; then
    echo "Test supervisor directory must not be a symlink" >&2
    exit 1
fi
mkdir -p "${supervisor_bin_dir}"
supervisor_bin_dir="$(cd "${supervisor_bin_dir}" && pwd -P)"
supervisor_stage="$(mktemp -d "${supervisor_bin_dir}/.download.XXXXXX")"
trap 'rm -rf -- "${supervisor_stage}"' EXIT

for supervisor_program in pueue pueued; do
    supervisor_asset="${supervisor_program}-${supervisor_arch}-${supervisor_platform}"
    supervisor_expected="$(awk -v asset="${supervisor_asset}" '$2 == asset { print $1 }' "${supervisor_manifest}")"
    if [[ ! "${supervisor_expected}" =~ ^[0-9a-f]{64}$ ]]; then
        echo "Missing or ambiguous pinned checksum for ${supervisor_asset}" >&2
        exit 1
    fi
    supervisor_destination="${supervisor_bin_dir}/${supervisor_program}"
    if [[ -L "${supervisor_destination}" ]]; then
        echo "Test supervisor executable must not be a symlink" >&2
        exit 1
    fi
    if [[ -f "${supervisor_destination}" ]] && [[ "$(supervisor_sha256 "${supervisor_destination}")" == "${supervisor_expected}" ]]; then
        chmod 0755 "${supervisor_destination}"
        echo "Verified cached ${supervisor_program} 4.0.4: ${supervisor_destination}"
        continue
    fi
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
        --output "${supervisor_stage}/${supervisor_program}" \
        "https://github.com/Nukesor/pueue/releases/download/v4.0.4/${supervisor_asset}"
    if [[ "$(supervisor_sha256 "${supervisor_stage}/${supervisor_program}")" != "${supervisor_expected}" ]]; then
        echo "Checksum mismatch for ${supervisor_asset}" >&2
        exit 1
    fi
    chmod 0755 "${supervisor_stage}/${supervisor_program}"
    mv -- "${supervisor_stage}/${supervisor_program}" "${supervisor_destination}"
    echo "Installed verified ${supervisor_program} 4.0.4: ${supervisor_destination}"
done
