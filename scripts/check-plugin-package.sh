#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
PLUGIN_ROOT="${REPO_ROOT}/plugins/delegation-layer"

[[ -d "${PLUGIN_ROOT}" ]] || { echo "ERROR: plugin directory is missing." >&2; exit 1; }
[[ -f "${PLUGIN_ROOT}/.codex-plugin/plugin.json" ]] || { echo "ERROR: Codex plugin manifest is missing." >&2; exit 1; }
[[ -f "${PLUGIN_ROOT}/.claude-plugin/plugin.json" ]] || { echo "ERROR: Claude plugin manifest is missing." >&2; exit 1; }
[[ ! -e "${PLUGIN_ROOT}/plugin.json" ]] || { echo "ERROR: root Agent Plugin manifest would bypass the Codex compatibility hook loader." >&2; exit 1; }
[[ -f "${REPO_ROOT}/.agents/plugins/marketplace.json" ]] || { echo "ERROR: Codex marketplace manifest is missing." >&2; exit 1; }
[[ -f "${REPO_ROOT}/.claude-plugin/marketplace.json" ]] || { echo "ERROR: Claude marketplace manifest is missing." >&2; exit 1; }

diff -ru "${REPO_ROOT}/skills/agent-integration" "${PLUGIN_ROOT}/skills/agent-integration"
cmp "${REPO_ROOT}/install.sh" "${PLUGIN_ROOT}/scripts/install-cli.sh"
[[ -x "${PLUGIN_ROOT}/scripts/ensure-delegate.sh" ]] || {
  echo "ERROR: plugin bootstrap is not executable." >&2
  exit 1
}

python3 - "${REPO_ROOT}" "${PLUGIN_ROOT}" <<'PY'
import json
import pathlib
import sys

repo_root = pathlib.Path(sys.argv[1])
plugin_root = pathlib.Path(sys.argv[2])

def read_json(path):
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)

def require(condition, message):
    if not condition:
        raise SystemExit(f"ERROR: {message}")

package = read_json(repo_root / "package.json")
codex = read_json(plugin_root / ".codex-plugin" / "plugin.json")
claude = read_json(plugin_root / ".claude-plugin" / "plugin.json")
codex_marketplace = read_json(repo_root / ".agents" / "plugins" / "marketplace.json")
claude_marketplace = read_json(repo_root / ".claude-plugin" / "marketplace.json")

for manifest in (codex, claude):
    require(manifest.get("name") == "delegation-layer", "plugin manifest name must be delegation-layer")
    require(manifest.get("version") == package["version"], "plugin version must match package version")

require(codex.get("name") == claude["name"], "plugin manifest names must agree")
require(codex.get("skills") == "./skills/", "Codex manifest must expose the bundled skills")
require(claude.get("skills") == "./skills/", "Claude manifest must expose the bundled skills")
require("hooks" not in claude, "Claude must use the shared hooks/hooks.json discovery path")
shared_hooks = plugin_root / "hooks" / "hooks.json"
require(shared_hooks.is_file(), "shared hook manifest is missing")
require(not (plugin_root / "hooks" / "claude-hooks.json").exists(), "duplicate Claude hook manifest must not be packaged")
hooks = read_json(shared_hooks)
command = hooks["hooks"]["SessionStart"][0]["hooks"][0]["command"]
require("CLAUDE_PLUGIN_ROOT" in command and "PLUGIN_ROOT" in command, "shared hook must support both harness plugin root variables")
require(command.startswith("/bin/sh -c '") and command.endswith("'"), "shared hook must invoke its bootstrap through POSIX sh")

for marketplace in (codex_marketplace, claude_marketplace):
    require(len(marketplace.get("plugins", [])) == 1, "marketplace must contain exactly one plugin")
    entry = marketplace["plugins"][0]
    require(entry.get("name") == "delegation-layer", "marketplace entry name must match the plugin")

require(codex_marketplace["plugins"][0]["source"]["path"] == "./plugins/delegation-layer", "Codex marketplace source path is stale")
require(claude_marketplace["plugins"][0]["source"] == "./plugins/delegation-layer", "Claude marketplace source path is stale")

for path in plugin_root.rglob("*"):
    require(not path.is_symlink(), f"plugin must not contain symlinks: {path}")

print("Plugin package checks passed.")
PY

"${SCRIPT_DIR}/test-plugin-bootstrap.sh"
