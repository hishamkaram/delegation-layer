#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BOOTSTRAP="${SCRIPT_DIR}/../plugins/delegation-layer/scripts/ensure-delegate.sh"
ROOT="$(mktemp -d)"
trap 'rm -rf "${ROOT}"' EXIT

PLUGIN_ROOT="${ROOT}/plugin"
PATH_BIN="${ROOT}/path-bin"
INSTALL_DIR="${ROOT}/installed"
SYSTEM_PATH="/usr/bin:/bin"
HOME="${ROOT}/home"
mkdir -p "${PLUGIN_ROOT}/scripts" "${PATH_BIN}" "${HOME}"
cp "${BOOTSTRAP}" "${PLUGIN_ROOT}/scripts/ensure-delegate.sh"
chmod 0755 "${PLUGIN_ROOT}/scripts/ensure-delegate.sh"

cat > "${PLUGIN_ROOT}/scripts/install-cli.sh" <<'INSTALLER'
#!/usr/bin/env sh
set -eu
mkdir -p "${DELEGATION_LAYER_INSTALL_DIR}"
printf '%s\n' installed > "${DELEGATION_LAYER_INSTALL_DIR}/installer-marker"
for binary in delegate delegate-run pueue pueued; do
  cat > "${DELEGATION_LAYER_INSTALL_DIR}/${binary}" <<'BIN'
#!/usr/bin/env sh
binary_name="$(basename "$0")"
if [ "${1:-}" = "--version" ]; then
  case "$binary_name" in
    delegate) printf '%s\n' 'delegate test' ;;
    pueue) printf '%s\n' 'pueue 4.0.4' ;;
    pueued) printf '%s\n' 'pueued 4.0.4' ;;
  esac
  exit 0
fi
exit 0
BIN
  chmod 0755 "${DELEGATION_LAYER_INSTALL_DIR}/${binary}"
done
INSTALLER
chmod 0755 "${PLUGIN_ROOT}/scripts/install-cli.sh"

cat > "${PATH_BIN}/delegate" <<'EXISTING'
#!/usr/bin/env sh
if [ "${1:-}" = "--version" ]; then
  printf '%s\n' 'delegate existing'
  exit 0
fi
exit 0
EXISTING
chmod 0755 "${PATH_BIN}/delegate"

cat > "${PATH_BIN}/delegate-run" <<'EXISTING_RUNNER'
#!/usr/bin/env sh
exit 0
EXISTING_RUNNER
chmod 0755 "${PATH_BIN}/delegate-run"

for binary in pueue pueued; do
  cat > "${PATH_BIN}/${binary}" <<BIN
#!/usr/bin/env sh
if [ "\${1:-}" = "--version" ]; then
  printf '%s\\n' '${binary} 4.0.4'
  exit 0
fi
exit 0
BIN
  chmod 0755 "${PATH_BIN}/${binary}"
done

env HOME="${HOME}" DELEGATION_LAYER_INSTALL_DIR= PATH="${PATH_BIN}:${SYSTEM_PATH}" PLUGIN_ROOT="${PLUGIN_ROOT}" \
  "${PLUGIN_ROOT}/scripts/ensure-delegate.sh" > "${ROOT}/existing.out"
[[ ! -f "${PATH_BIN}/installer-marker" ]] || {
  echo "ERROR: valid existing delegate triggered a reinstall." >&2
  exit 1
}

rm "${PATH_BIN}/delegate"
env HOME="${HOME}" DELEGATION_LAYER_INSTALL_DIR="${PATH_BIN}" PATH="${PATH_BIN}:${SYSTEM_PATH}" PLUGIN_ROOT="${PLUGIN_ROOT}" \
  "${PLUGIN_ROOT}/scripts/ensure-delegate.sh" > "${ROOT}/installed.out"
[[ -f "${PATH_BIN}/installer-marker" ]] || {
  echo "ERROR: missing delegate did not trigger bootstrap." >&2
  exit 1
}
[[ -x "${PATH_BIN}/delegate" ]] || {
  echo "ERROR: bootstrap did not install delegate on PATH." >&2
  exit 1
}

RELATIVE_BIN="${ROOT}/relative-bin"
mkdir -p "${RELATIVE_BIN}"
set +e
(
  cd "${ROOT}"
  env HOME="${HOME}" DELEGATION_LAYER_INSTALL_DIR= \
    PATH="relative-bin:${SYSTEM_PATH}" PLUGIN_ROOT="${PLUGIN_ROOT}" \
    "${PLUGIN_ROOT}/scripts/ensure-delegate.sh" > "${ROOT}/relative.out" 2>&1
)
status=$?
set -e
[[ ${status} -eq 1 ]] || {
  echo "ERROR: bootstrap installed into an untrusted relative PATH entry." >&2
  exit 1
}
[[ ! -f "${RELATIVE_BIN}/installer-marker" ]] || {
  echo "ERROR: bootstrap wrote into an untrusted relative PATH entry." >&2
  exit 1
}
grep -q 'not on PATH' "${ROOT}/relative.out" || {
  echo "ERROR: default private install PATH guidance is missing." >&2
  exit 1
}
rm -rf "${HOME}/.local"

SHADOW_DIR="${ROOT}/shadow"
SHADOW_INSTALL_DIR="${ROOT}/shadow-install"
mkdir -p "${SHADOW_DIR}" "${SHADOW_INSTALL_DIR}"
cat > "${SHADOW_DIR}/delegate" <<'SHADOW'
#!/usr/bin/env sh
printf '%s\n' 'stale delegate'
exit 1
SHADOW
chmod 0755 "${SHADOW_DIR}/delegate"
set +e
env HOME="${HOME}" DELEGATION_LAYER_INSTALL_DIR="${SHADOW_INSTALL_DIR}" \
  PATH="${SHADOW_DIR}:${SHADOW_INSTALL_DIR}:${SYSTEM_PATH}" PLUGIN_ROOT="${PLUGIN_ROOT}" \
  "${PLUGIN_ROOT}/scripts/ensure-delegate.sh" > "${ROOT}/shadow-error.out" 2>&1
status=$?
set -e
[[ ${status} -eq 1 ]] || {
  echo "ERROR: bootstrap accepted a shadowed delegate." >&2
  exit 1
}
[[ -x "${SHADOW_INSTALL_DIR}/delegate" ]] || {
  echo "ERROR: bootstrap did not install the bundle behind the shadowing command." >&2
  exit 1
}
grep -q 'stale delegate' "${SHADOW_DIR}/delegate" || {
  echo "ERROR: bootstrap overwrote the shadowing delegate." >&2
  exit 1
}
grep -q 'resolves delegate to' "${ROOT}/shadow-error.out" || {
  echo "ERROR: shadowing PATH guidance is missing." >&2
  exit 1
}

rm -f "${PATH_BIN}/delegate" "${PATH_BIN}/delegate-run" "${PATH_BIN}/pueue" "${PATH_BIN}/pueued" "${PATH_BIN}/installer-marker"
rm -rf "${INSTALL_DIR}"
set +e
env HOME="${HOME}" DELEGATION_LAYER_INSTALL_DIR="${INSTALL_DIR}" \
  PATH="${PATH_BIN}:${SYSTEM_PATH}" PLUGIN_ROOT="${PLUGIN_ROOT}" \
  "${PLUGIN_ROOT}/scripts/ensure-delegate.sh" > "${ROOT}/path-error.out" 2>&1
status=$?
set -e
[[ ${status} -eq 1 ]] || {
  echo "ERROR: bootstrap did not reject an installation outside PATH." >&2
  exit 1
}
grep -q 'not on PATH' "${ROOT}/path-error.out" || {
  echo "ERROR: PATH repair guidance is missing." >&2
  exit 1
}

echo "Plugin bootstrap checks passed."
