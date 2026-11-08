"""Observe the toolchains and static inputs a development candidate is built with.

Versions come from the tools' own output, never from a pin file alone. The
source commit pins Go; the observed `go` must be that version and must be the
binary under its own GOROOT, so a stale tool earlier on PATH is refused.
"""

from __future__ import annotations

from pathlib import Path
import shutil

from scripts.acceptance.evidence.digests import sha
from scripts.release_verification.candidate_build_artifacts import BuildContext
from scripts.release_verification.candidate_build_artifacts import run
from scripts.release_verification.candidate_build_plan import STAGE
from scripts.release_verification.candidate_build_plan import build_inputs
from scripts.release_verification.candidate_build_plan import parse_go_pin
from scripts.release_verification.candidate_build_plan import parse_go_version
from scripts.release_verification.candidate_build_plan import parse_goreleaser_version
from scripts.release_verification.candidate_build_plan import parse_node_version
from scripts.release_verification.candidate_build_plan import parse_python_version
from scripts.release_verification.candidate_build_plan import require_pinned_go
from scripts.release_verification.candidate_manifest import BuildInputs
from scripts.acceptance.evidence.source import MAX_SOURCE_FILE
from scripts.acceptance.evidence.source import VerificationError
from scripts.acceptance.evidence.source import _bounded_file_bytes


VERSION_TIMEOUT_SECONDS = 60


def _version_output(context: BuildContext, name: str, argv: list[str]) -> bytes:
    return run(context, name, argv, cwd=context.clone, timeout=VERSION_TIMEOUT_SECONDS, capture=True)


def _require_go_under_goroot(context: BuildContext) -> None:
    goroot = _version_output(context, "go-env-goroot", ["go", "env", "GOROOT"]).decode(
        "utf-8", errors="replace").strip()
    resolved = shutil.which("go", path=context.environment["PATH"])
    if (not goroot or resolved is None
            or Path(resolved).resolve() != (Path(goroot) / "bin" / "go").resolve()):
        raise VerificationError("go_root_path_mismatch", STAGE)


def _static_digest(context: BuildContext, relative: str) -> str:
    return sha(_bounded_file_bytes(context.clone / relative, MAX_SOURCE_FILE, "build_input_unreadable"))


def observe_build_inputs(context: BuildContext, *, python: str) -> BuildInputs:
    """Observe all four toolchains and the static configuration of the source clone."""
    pin = parse_go_pin(_bounded_file_bytes(context.clone / ".go-version", 64, "go_pin_unreadable"))
    go = parse_go_version(_version_output(context, "go-version", ["go", "version"]))
    require_pinned_go(go, pin)
    _require_go_under_goroot(context)
    return build_inputs(
        config_sha256=_static_digest(context, ".goreleaser.yaml"),
        go_sum_sha256=_static_digest(context, "go.sum"),
        go=go,
        goreleaser=parse_goreleaser_version(_version_output(
            context, "goreleaser-version", [str(context.goreleaser), "--version"])),
        python=parse_python_version(_version_output(context, "python-version", [python, "--version"])),
        node=parse_node_version(_version_output(context, "node-version", ["node", "--version"])),
    )
