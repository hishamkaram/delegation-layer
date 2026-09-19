#!/usr/bin/env sh
set -eu

REPOSITORY="hishamkaram/delegation-layer"
REQUESTED_VERSION="${DELEGATION_LAYER_VERSION:-latest}"
INSTALL_DIR="${DELEGATION_LAYER_INSTALL_DIR:-$HOME/.local/bin}"

fail() {
  printf 'delegation-layer: %s\n' "$1" >&2
  exit 1
}

for command_name in awk curl grep install mktemp tar uname; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command not found: $command_name"
done

if [ "$REQUESTED_VERSION" = "latest" ]; then
  latest_url="https://github.com/${REPOSITORY}/releases/latest"
  resolved_url="$(curl -fsSL -o /dev/null -w '%{url_effective}' "$latest_url")" || fail "could not resolve the latest release"
  VERSION="${resolved_url##*/}"
else
  VERSION="$REQUESTED_VERSION"
  case "$VERSION" in
    v*) ;;
    *) VERSION="v${VERSION}" ;;
  esac
fi

printf '%s\n' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || \
  fail "invalid version: $VERSION"

case "$(uname -s)" in
  Linux) OS=Linux ;;
  Darwin) OS=Darwin ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

ARCHIVE="delegation-layer_${VERSION#v}_${OS}_${ARCH}.tar.gz"
RELEASE_URL="https://github.com/${REPOSITORY}/releases/download/${VERSION}"
TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TEMP_DIR"' EXIT

curl -fsSL --retry 3 -o "$TEMP_DIR/$ARCHIVE" "$RELEASE_URL/$ARCHIVE" || \
  fail "could not download $ARCHIVE"
curl -fsSL --retry 3 -o "$TEMP_DIR/checksums.txt" "$RELEASE_URL/checksums.txt" || \
  fail "could not download checksums.txt"

CHECKSUM_LINE="$(awk -v archive="$ARCHIVE" '$2 == archive { print; exit }' "$TEMP_DIR/checksums.txt")"
[ -n "$CHECKSUM_LINE" ] || fail "checksums.txt does not contain $ARCHIVE"

cd "$TEMP_DIR"
if command -v sha256sum >/dev/null 2>&1; then
  printf '%s\n' "$CHECKSUM_LINE" | sha256sum -c - || fail "checksum verification failed"
elif command -v shasum >/dev/null 2>&1; then
  printf '%s\n' "$CHECKSUM_LINE" | shasum -a 256 -c - || fail "checksum verification failed"
else
  fail "sha256sum or shasum is required for checksum verification"
fi

tar -xzf "$ARCHIVE" -C "$TEMP_DIR" || fail "could not extract $ARCHIVE"
mkdir -p "$INSTALL_DIR"
for binary in delegate delegate-run pueue pueued; do
  [ -f "$TEMP_DIR/$binary" ] || fail "release archive does not contain $binary"
  install -m 0755 "$TEMP_DIR/$binary" "$INSTALL_DIR/$binary"
done

printf 'Installed delegation-layer CLI and bundled supervisor %s in %s\n' "$VERSION" "$INSTALL_DIR"
case ":${PATH:-}:" in
  *:"$INSTALL_DIR":*) ;;
  *) printf 'Add %s to PATH if it is not already there.\n' "$INSTALL_DIR" ;;
esac
