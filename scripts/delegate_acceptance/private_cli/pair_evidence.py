"""Private CLI pair evidence boundary."""

import hashlib
import json


def assert_state_supervisor_pair(state, bundled_directory):
    supervisor = state / ".supervisor"
    manifest_path = supervisor / "pueue-pair.json"
    if not manifest_path.is_file() or manifest_path.is_symlink():
        raise RuntimeError("state-root supervisor pair manifest is missing or unsafe")
    try:
        manifest = json.loads(manifest_path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        raise RuntimeError("state-root supervisor pair manifest is unreadable: " + str(error)) from error
    client_digest = manifest.get("client_sha256", "")
    daemon_digest = manifest.get("daemon_sha256", "")
    pair_digest = hashlib.sha256((client_digest + "\n" + daemon_digest).encode()).hexdigest()
    if (
        manifest.get("schema_version") != 1
        or manifest.get("pair_sha256") != pair_digest
        or len(client_digest) != 64
        or len(daemon_digest) != 64
    ):
        raise RuntimeError("state-root supervisor pair manifest has an invalid identity")
    pair_directory = supervisor / ("pueue-pair-" + pair_digest)
    for name, expected_digest in (("pueue", client_digest), ("pueued", daemon_digest)):
        installed = pair_directory / name
        bundled = bundled_directory / name
        if (
            installed.is_symlink()
            or not installed.is_file()
            or hashlib.sha256(installed.read_bytes()).hexdigest() != expected_digest
            or hashlib.sha256(bundled.read_bytes()).hexdigest() != expected_digest
        ):
            raise RuntimeError("state-root pair does not match its packaged supervisor member " + name)
    return pair_directory
