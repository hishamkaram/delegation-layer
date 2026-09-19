#!/usr/bin/env bash
set -euo pipefail

OUTPUT_ROOT="${1:-.release/pueue}"
PUEUE_VERSION="4.0.4"
RELEASE_URL="https://github.com/Nukesor/pueue/releases/download/v${PUEUE_VERSION}"
TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TEMP_DIR}"' EXIT

download() {
    local os="$1"
    local arch="$2"
    local asset="$3"
    local digest="$4"
    local destination="${OUTPUT_ROOT}/${os}/${arch}/$(basename "${asset%%-*}")"
    local asset_path="${TEMP_DIR}/$(basename "${asset}")"

    mkdir -p "$(dirname "${destination}")"
    curl -fsSL --retry 3 -o "${asset_path}" "${RELEASE_URL}/${asset}"
    if [ "${CHECKSUM_TOOL}" = "sha256sum" ]; then
        printf '%s  %s\n' "${digest}" "${asset_path}" | sha256sum -c -
    else
        printf '%s  %s\n' "${digest}" "${asset_path}" | shasum -a 256 -c -
    fi
    install -m 0755 "${asset_path}" "${destination}"
}

command -v curl >/dev/null 2>&1 || { echo 'curl is required' >&2; exit 1; }
command -v install >/dev/null 2>&1 || { echo 'install is required' >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
    CHECKSUM_TOOL=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    CHECKSUM_TOOL='shasum -a 256'
else
    echo 'sha256sum or shasum is required' >&2
    exit 1
fi

# The bundle is pinned for reproducible release archives. Runtime admission
# does not compare this version and accepts compatible supervisor behavior.
download darwin amd64 pueue-x86_64-apple-darwin c5b89a2f9f9d355b33880735a1f12a8b8d09002f95cc7d2e5f3294e313fb8540
download darwin amd64 pueued-x86_64-apple-darwin 4de7c4790989198007c6c0789c56606237d0456a2ce3666b5a0b198478c06263
download darwin arm64 pueue-aarch64-apple-darwin 7780dbd21e3a4106e88a57396d6dc2dbcbbae253c7e00d92d994902896b8cb82
download darwin arm64 pueued-aarch64-apple-darwin bf5a1151d70c328dd036fb6d786fba53a68d8574c12391d04db3d08b04079205
download linux amd64 pueue-x86_64-unknown-linux-musl c1b10d7e4e62211075ddd0e1dc3e8cbfc5a43d662cb3be7402a28504e23fcb51
download linux amd64 pueued-x86_64-unknown-linux-musl 5afeff6adbafb909e8d54e2caff158e6966c2adffa2c09e60fd631cc51b60390
download linux arm64 pueue-aarch64-unknown-linux-musl 759bf5100a51024997111c6913aaf3330a0cdfd893ff552dcf429ae9b5e01e09
download linux arm64 pueued-aarch64-unknown-linux-musl 332c5ef74270b64aeaf04894c8c04826f3422eb7d50dbd1a8e0706d74a42f653

echo "Prepared bundled Pueue binaries in ${OUTPUT_ROOT}"
