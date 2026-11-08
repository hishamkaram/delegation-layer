"""Finite manifests and installed-bootstrap observations for distribution tests."""

from __future__ import annotations

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[2])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]



from scripts.acceptance.evidence.digests import sha
from scripts.release_verification.candidate_install_contract import BOOTSTRAP_OUTCOME
from scripts.release_verification.candidate_install_contract import BOOTSTRAP_SHUTDOWN
from scripts.release_verification.candidate_install_contract import BOOTSTRAP_TERMINATION_CAUSE
from scripts.release_verification.candidate_install_contract import BOOTSTRAP_WORKER_RESULT
from scripts.release_verification.candidate_install_contract import BootstrapObservation
from scripts.release_verification.candidate_manifest import ArtifactEntry
from scripts.release_verification.candidate_manifest import BuildInputs
from scripts.release_verification.candidate_manifest import CandidateLocator
from scripts.release_verification.candidate_manifest import CandidateManifest
from scripts.release_verification.candidate_manifest import SourceIdentity
from scripts.release_verification.candidate_manifest import Toolchains
from scripts.release_verification.candidate_manifest import encode_manifest
from scripts.release_verification.candidate_manifest import expected_inventory
from scripts.release_verification.contract import InstalledExecutables


VERSION = "0.1.37"
COMMIT = "a" * 40
TREE = "b" * 40
BASE = "c" * 40
REPOSITORY = "hishamkaram/delegation-layer"
NATIVE_BINARIES = InstalledExecutables(*(Path("/candidate") / name for name in ("delegate", "delegate-run", "pueue", "pueued")))
BOOTSTRAP_ROOT_ID = "1" * 32
BOOTSTRAP_TASK_ID = "2" * 32


def manifest(version: str = VERSION) -> CandidateManifest:
    artifacts = tuple(
        ArtifactEntry(path=path, kind=kind, os=os_name, arch=arch, size=1000 + index,
                      sha256=sha(path.encode()))
        for index, (path, kind, os_name, arch) in enumerate(expected_inventory(version)))
    return CandidateManifest(
        version=version, build_tag="v" + version,
        source=SourceIdentity(commit=COMMIT, tree=TREE, base_commit=BASE),
        build=BuildInputs(goreleaser_config_sha256="1" * 64,
                          go_sum_sha256="2" * 64,
                          toolchains=Toolchains(go="1.27.3", goreleaser="2.18.1",
                                                python="3.12.13", node="22.11.0")),
        artifacts=artifacts,
    )


def locator_for(value: CandidateManifest) -> CandidateLocator:
    return CandidateLocator(repository=REPOSITORY, run_id=901, run_attempt=1,
                            artifact_id=77, artifact_archive_sha256="f" * 64,
                            manifest_sha256=sha(encode_manifest(value)))


def bootstrap(binaries: InstalledExecutables = NATIVE_BINARIES) -> BootstrapObservation:
    """The closed installed fixture turn an accepted install of ``binaries`` records."""
    return BootstrapObservation(
        root_id=BOOTSTRAP_ROOT_ID, task_id=BOOTSTRAP_TASK_ID, binaries=binaries, outcome=BOOTSTRAP_OUTCOME,
        worker_result=BOOTSTRAP_WORKER_RESULT, termination_cause=BOOTSTRAP_TERMINATION_CAUSE, scope_empty=True,
        shutdown=BOOTSTRAP_SHUTDOWN)
