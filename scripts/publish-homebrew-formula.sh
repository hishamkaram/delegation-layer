#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The renderer owns tag and repository validation, archive names, checksum
# parsing, and the formula template.
# shellcheck source=render-homebrew-formula.sh
source "${SCRIPT_DIR}/render-homebrew-formula.sh"

tag=${1:?usage: publish-homebrew-formula.sh vX.Y.Z}
tap_repository=${TAP_REPOSITORY:-hishamkaram/homebrew-tap}
formula_path=${FORMULA_PATH:-Formula/delegation-layer.rb}

validate_tag "${tag}"
owner_repo=$(resolve_repository)
if [[ -z ${HOMEBREW_TAP_TOKEN:-} ]]; then
    printf 'HOMEBREW_TAP_TOKEN is required\n' >&2
    exit 1
fi
for command in awk base64 curl gh grep mktemp tr; do
    if ! command -v "${command}" >/dev/null 2>&1; then
        printf '%s is required\n' "${command}" >&2
        exit 1
    fi
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    printf 'sha256sum or shasum is required\n' >&2
    exit 1
fi

temp_dir=$(mktemp -d)
trap 'rm -rf "${temp_dir}"' EXIT
checksums="${temp_dir}/checksums.txt"
formula="${temp_dir}/delegation-layer.rb"
source_token=${GH_TOKEN:-${HOMEBREW_TAP_TOKEN}}

GH_TOKEN="${source_token}" gh api "repos/${owner_repo}/releases/tags/${tag}" \
    --jq '.tag_name' | grep -Fx "${tag}" >/dev/null
source_commit=$(GH_TOKEN="${source_token}" gh api \
    "repos/${owner_repo}/commits/${tag}" --jq .sha)
if [[ ! "${source_commit}" =~ ^[[:xdigit:]]{40}$ ]]; then
    printf 'could not resolve the promoted tag commit for %s\n' "${tag}" >&2
    exit 1
fi
curl --fail --location --retry 3 --silent --show-error \
    "https://github.com/${owner_repo}/releases/download/${tag}/checksums.txt" \
    --output "${checksums}"

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

verify_archive() {
    local archive="$1"
    local expected="$2"
    local path="${temp_dir}/${archive}"
    local actual
    curl --fail --location --retry 3 --silent --show-error \
        "https://github.com/${owner_repo}/releases/download/${tag}/${archive}" \
        --output "${path}"
    actual=$(sha256_file "${path}")
    if [[ "${actual}" != "${expected}" ]]; then
        printf 'checksum mismatch for %s\n' "${archive}" >&2
        exit 1
    fi
    GH_TOKEN="${source_token}" gh attestation verify "${path}" \
        --repo "${owner_repo}" \
        --signer-workflow "${owner_repo}/.github/workflows/release.yml" \
        --source-ref "refs/tags/${tag}" \
        --source-digest "${source_commit}" \
        >/dev/null
}

digests=$(release_digests "${tag}" "${checksums}")
while read -r archive digest; do
    verify_archive "${archive}" "${digest}"
done <<<"${digests}"

render_formula "${tag}" "${checksums}" >"${formula}"

encoded=$(base64 <"${formula}" | tr -d '\n')
current_sha=$(GH_TOKEN="${HOMEBREW_TAP_TOKEN}" gh api \
    "repos/${tap_repository}/contents/${formula_path}" --jq .sha 2>/dev/null || true)
current_content=$(GH_TOKEN="${HOMEBREW_TAP_TOKEN}" gh api \
    --header 'Accept: application/vnd.github.raw+json' \
    "repos/${tap_repository}/contents/${formula_path}" 2>/dev/null || true)
if [[ "${current_content}" == "$(cat "${formula}")" ]]; then
    printf 'Homebrew formula already up to date for %s\n' "${tag}"
    exit 0
fi

arguments=(
    api --method PUT "repos/${tap_repository}/contents/${formula_path}"
    --raw-field "message=brew: update delegation-layer to ${tag}"
    --raw-field "content=${encoded}"
)
if [[ -n "${current_sha}" ]]; then
    arguments+=(--raw-field "sha=${current_sha}")
fi
GH_TOKEN="${HOMEBREW_TAP_TOKEN}" gh "${arguments[@]}" >/dev/null
printf 'Published Homebrew formula for %s\n' "${tag}"
