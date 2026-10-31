#!/usr/bin/env bash
# Canonical Homebrew formula policy: tag and repository format, release archive
# names, checksum parsing, and the formula template. It touches no network,
# credentials, or release binary.
#
# Run:    render-homebrew-formula.sh vX.Y.Z CHECKSUMS_FILE   (formula on stdout)
set -euo pipefail

default_repository=hishamkaram/delegation-layer
archive_platforms=(Darwin_amd64 Darwin_arm64 Linux_amd64 Linux_arm64)

validate_tag() {
    if [[ ! "$1" =~ ^v[0-9]+(\.[0-9]+){2}$ ]]; then
        printf 'stable semantic version tag required, got %q\n' "$1" >&2
        exit 1
    fi
}

# Prints the effective owner/repository: DELEGATION_LAYER_REPOSITORY or the default.
resolve_repository() {
    local repository=${DELEGATION_LAYER_REPOSITORY:-${default_repository}}
    if [[ ! "${repository}" =~ ^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_][A-Za-z0-9._-]*$ ]]; then
        printf 'owner/repository required, got %q\n' "${repository}" >&2
        exit 1
    fi
    printf '%s' "${repository}"
}

# Command substitutions run with errexit off, so each caller adds "|| exit 1".
# Prints the lowercase digest of the single checksum entry named for the archive.
checksum_for() {
    local archive="$1"
    local checksums="$2"
    local matches fields digest
    matches=$(awk -v archive="${archive}" '$2 == archive { print NF, $1 }' <"${checksums}")
    if [[ -z "${matches}" ]]; then
        printf 'missing checksum for %s\n' "${archive}" >&2
        exit 1
    fi
    if [[ "${matches}" == *$'\n'* ]]; then
        printf 'duplicate checksum entries for %s\n' "${archive}" >&2
        exit 1
    fi
    fields=${matches%% *}
    digest=${matches#* }
    if [[ "${fields}" != 2 || ! "${digest}" =~ ^[[:xdigit:]]{64}$ ]]; then
        printf 'malformed checksum for %s\n' "${archive}" >&2
        exit 1
    fi
    printf '%s' "${digest}" | tr '[:upper:]' '[:lower:]'
}

# Prints one "<archive> <sha256>" line per supported platform, in the order of
# archive_platforms. Each archive needs exactly one entry; entries for other
# assets are ignored, and archives may share a digest.
release_digests() {
    local tag="$1"
    local checksums="$2"
    local platform archive digest
    if [[ ! -f "${checksums}" || ! -r "${checksums}" ]]; then
        printf 'readable checksums file required, got %q\n' "${checksums}" >&2
        exit 1
    fi
    for platform in "${archive_platforms[@]}"; do
        archive="delegation-layer_${tag#v}_${platform}.tar.gz"
        digest=$(checksum_for "${archive}" "${checksums}") || exit 1
        printf '%s %s\n' "${archive}" "${digest}"
    done
}

# Validates every input, then writes the formula to stdout.
render_formula() {
    local tag="$1"
    local checksums="$2"
    local version=${tag#v}
    local owner_repo digests
    local darwin_amd64 darwin_amd64_sha256 darwin_arm64 darwin_arm64_sha256
    local linux_amd64 linux_amd64_sha256 linux_arm64 linux_arm64_sha256
    validate_tag "${tag}"
    owner_repo=$(resolve_repository) || exit 1
    digests=$(release_digests "${tag}" "${checksums}") || exit 1
    {
        read -r darwin_amd64 darwin_amd64_sha256
        read -r darwin_arm64 darwin_arm64_sha256
        read -r linux_amd64 linux_amd64_sha256
        read -r linux_arm64 linux_arm64_sha256
    } <<<"${digests}"

    cat <<EOF
class DelegationLayer < Formula
  desc "Durable supervised delegation for supported AI CLIs"
  homepage "https://github.com/${owner_repo}"
  version "${version}"
  license "MIT"

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
    libexec.install "pueue", "pueued"
  end

  test do
    assert_match "delegate #{version}", shell_output("#{bin}/delegate version")
    system bin/"delegate", "providers", "--json"
    assert_match "pueue ", shell_output("#{libexec}/pueue --version")
    assert_match "pueued ", shell_output("#{libexec}/pueued --version")
  end
end
EOF
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    if [[ $# -ne 2 ]]; then
        printf 'usage: render-homebrew-formula.sh vX.Y.Z CHECKSUMS_FILE\n' >&2
        exit 2
    fi
    render_formula "$1" "$2"
fi
