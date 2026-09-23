#!/usr/bin/env bash
set -euo pipefail

tag=${1:?usage: publish-homebrew-formula.sh vX.Y.Z}
tap_repository=${TAP_REPOSITORY:-hishamkaram/homebrew-tap}
formula_path=${FORMULA_PATH:-Formula/delegation-layer.rb}
owner_repo=${DELEGATION_LAYER_REPOSITORY:-hishamkaram/delegation-layer}

if [[ ! "${tag}" =~ ^v[0-9]+(\.[0-9]+){2}$ ]]; then
    printf 'stable semantic version tag required, got %q\n' "${tag}" >&2
    exit 1
fi
if [[ -z ${HOMEBREW_TAP_TOKEN:-} ]]; then
    printf 'HOMEBREW_TAP_TOKEN is required\n' >&2
    exit 1
fi
for command in awk base64 curl gh grep mktemp; do
    if ! command -v "${command}" >/dev/null 2>&1; then
        printf '%s is required\n' "${command}" >&2
        exit 1
    fi
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    printf 'sha256sum or shasum is required\n' >&2
    exit 1
fi

version=${tag#v}
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

checksum_for() {
    local archive="$1"
    local digest
    digest=$(awk -v archive="${archive}" '$2 == archive { print $1; exit }' "${checksums}")
    if [[ ! "${digest}" =~ ^[[:xdigit:]]{64}$ ]]; then
        printf 'missing or invalid checksum for %s\n' "${archive}" >&2
        exit 1
    fi
    printf '%s' "${digest}"
}

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

darwin_amd64="delegation-layer_${version}_Darwin_amd64.tar.gz"
darwin_arm64="delegation-layer_${version}_Darwin_arm64.tar.gz"
linux_amd64="delegation-layer_${version}_Linux_amd64.tar.gz"
linux_arm64="delegation-layer_${version}_Linux_arm64.tar.gz"
darwin_amd64_sha256="$(checksum_for "${darwin_amd64}")"
darwin_arm64_sha256="$(checksum_for "${darwin_arm64}")"
linux_amd64_sha256="$(checksum_for "${linux_amd64}")"
linux_arm64_sha256="$(checksum_for "${linux_arm64}")"
verify_archive "${darwin_amd64}" "${darwin_amd64_sha256}"
verify_archive "${darwin_arm64}" "${darwin_arm64_sha256}"
verify_archive "${linux_amd64}" "${linux_amd64_sha256}"
verify_archive "${linux_arm64}" "${linux_arm64_sha256}"

cat >"${formula}" <<EOF
class DelegationLayer < Formula
  desc "Durable supervised delegation for supported AI CLIs"
  homepage "https://github.com/${owner_repo}"
  version "${version}"
  license "MIT"

  depends_on "pueue"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/${owner_repo}/releases/download/${tag}/${darwin_arm64}"
      sha256 "${darwin_arm64_sha256}"
    else
      url "https://github.com/${owner_repo}/releases/download/${tag}/${darwin_amd64}"
      sha256 "${darwin_amd64_sha256}"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/${owner_repo}/releases/download/${tag}/${linux_arm64}"
      sha256 "${linux_arm64_sha256}"
    else
      url "https://github.com/${owner_repo}/releases/download/${tag}/${linux_amd64}"
      sha256 "${linux_amd64_sha256}"
    end
  end

  def install
    bin.install "delegate"
    bin.install "delegate-run"
  end

  test do
    assert_match "delegate #{version}", shell_output("#{bin}/delegate version")
    system bin/"delegate", "providers", "--json"
  end
end
EOF

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
