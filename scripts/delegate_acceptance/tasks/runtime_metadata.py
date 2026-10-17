"""Pure runtime capability and launch identity checks over supplied evidence."""

from __future__ import annotations

import unicodedata
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, _hex_digest, require
from scripts.delegate_acceptance.paths.policy import _raw_clean_absolute_path


MAX_PROVIDER_RUNTIME_EVIDENCE_VERSION_BYTES = 64 * 1024


def runtime_capability_sha(definition: dict[str, object]) -> str:
    """Hash runtime capability fields while excluding executable identity."""
    runtime = definition.get("runtime")
    require(isinstance(runtime, dict), "runtime capability definition is missing runtime fields")

    def go_slice(value: object) -> object:
        # The Go digest clones slices into nil-backed values, so an empty
        # slice is encoded as JSON null. Keep this mirror byte-identical.
        return None if isinstance(value, list) and not value else value

    contract = {
        "revision": definition["revision"],
        "directory": definition["directory"],
        "environment": go_slice(definition["environment"]),
        "output_limit": definition["output_limit"],
        "help_args": go_slice(runtime["help_args"]),
        "required_flags": go_slice(runtime["required_flags"]),
    }
    return sha(canonical_go_json(contract))


def _provider_runtime_identity(evidence: object, phase: str,
                               label: str) -> dict[str, str]:
    require(isinstance(evidence, dict), f"{label} {phase} evidence is not an object")
    executable = evidence.get("provider_executable")
    version = evidence.get("provider_version")
    sha256 = evidence.get("provider_sha256")
    require(_raw_clean_absolute_path(executable),
            f"{label} {phase} executable is invalid")
    require(isinstance(version, str) and bool(version.strip()),
            f"{label} {phase} version is invalid")
    try:
        version_bytes = version.encode("utf-8")
    except UnicodeEncodeError as error:
        raise AcceptanceFailure(f"{label} {phase} version is not valid UTF-8") from error
    require(len(version_bytes) <= MAX_PROVIDER_RUNTIME_EVIDENCE_VERSION_BYTES,
            f"{label} {phase} version exceeds the terminal evidence bound")
    require(not any(unicodedata.category(character) == "Cc" for character in version),
            f"{label} {phase} version contains control characters")
    require(_hex_digest(sha256), f"{label} {phase} digest is invalid")
    return {"executable": executable, "version": version, "sha256": sha256}


def validate_provider_runtime_metadata(
        meta: dict[str, object], *, observed_version: str | None = None,
        launch_evidence: tuple[dict[str, object], dict[str, object]] | None = None,
        started_evidence: dict[str, object] | None = None,
        label: str = "provider runtime metadata") -> dict[str, str] | None:
    """Validate pending or historical runtime metadata and terminal identity."""
    require(isinstance(meta, dict), f"{label} is not an object")
    require(not (launch_evidence is not None and started_evidence is not None),
            f"{label} launch evidence modes are mutually exclusive")
    executable = meta.get("provider_executable")
    require(_raw_clean_absolute_path(executable),
            f"{label} executable is invalid")
    pending = "runtime_preflight_sha256" in meta
    version = meta.get("provider_version")
    if pending:
        require(_hex_digest(meta.get("runtime_preflight_sha256")),
                f"{label} preflight digest is invalid")
        require(version == "", f"{label} pending version is not empty")
        require(meta.get("environment_recorded") is True,
                f"{label} pending environment is not recorded")
    else:
        require(isinstance(version, str) and bool(version.strip()),
                f"{label} version is invalid")
        if observed_version is not None:
            require(version == observed_version,
                    f"{label} version does not match observed runtime")
    if "environment_recorded" in meta:
        require(type(meta["environment_recorded"]) is bool,
                f"{label} environment marker is invalid")
    if started_evidence is not None:
        identity = _provider_runtime_identity(started_evidence, "started", label)
        if not pending:
            require(executable == identity["executable"] and version == identity["version"],
                    f"{label} metadata does not match started identity")
        return identity
    if launch_evidence is None:
        return None
    started, exited = launch_evidence
    started_identity = _provider_runtime_identity(started, "started", label)
    exit_identity = _provider_runtime_identity(exited, "exit", label)
    require(started_identity == exit_identity,
            f"{label} started and exit runtime identities differ")
    identity = started_identity
    if not pending:
        require(executable == identity["executable"] and version == identity["version"],
                f"{label} metadata does not match launch identity")
    return identity
