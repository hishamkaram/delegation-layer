"""Build one immutable development candidate from a clean, committed checkout.

The caller's checkout is verified, never modified. All work happens in an owned
scratch directory holding an independent clone; the seven candidate files and
the manifest land in an owned payload directory that survives only on success or
when closure of a child is unproven.
Publication, remote tags, attestations and credentials are out of scope.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.digests import sha
from scripts.release_verification.assets import remember_directory
from scripts.release_verification.candidate_build_artifacts import BuildContext
from scripts.release_verification.candidate_build_artifacts import build_release_files
from scripts.release_verification.candidate_build_artifacts import collect_goreleaser_files
from scripts.release_verification.candidate_build_artifacts import observed_files
from scripts.release_verification.candidate_build_artifacts import pack_npm
from scripts.release_verification.candidate_build_artifacts import render_formula
from scripts.release_verification.candidate_build_artifacts import verify_bundles
from scripts.release_verification.candidate_build_artifacts import write_new
from scripts.release_verification.candidate_build_finalize import BuildOwners
from scripts.release_verification.candidate_build_finalize import settle
from scripts.release_verification.candidate_build_plan import STAGE
from scripts.release_verification.candidate_build_plan import BuildRequest
from scripts.release_verification.candidate_build_plan import build_manifest
from scripts.release_verification.candidate_build_plan import require_package_version
from scripts.acceptance.evidence.source import child_environment
from scripts.release_verification.candidate_build_source import clone_frozen
from scripts.release_verification.candidate_build_source import preflight_source
from scripts.release_verification.candidate_build_tools import observe_build_inputs
from scripts.release_verification.candidate_manifest import MANIFEST_PATH
from scripts.release_verification.candidate_manifest import CandidateManifest
from scripts.release_verification.candidate_manifest import encode_manifest
from scripts.release_verification.candidate_manifest import parse_manifest
from scripts.release_verification.candidate_manifest import payload_paths
from scripts.acceptance.evidence.source import MAX_SOURCE_FILE
from scripts.acceptance.evidence.source import VerificationError
from scripts.acceptance.evidence.source import _bounded_file_bytes


@dataclass(frozen=True)
class BuildPaths:
    source_root: Path
    work_dir: Path
    output_dir: Path
    receipt_path: Path
    goreleaser: Path
    python: str


def new_build_receipt(request: BuildRequest) -> dict:
    return {
        "schema_version": 1, "kind": "development-candidate-build", "verdict": "in_progress",
        "stage": "initialize", "version": request.version, "commit": request.commit,
        "base_commit": request.base_commit, "commands": [], "artifact": {},
        "cleanup": {"status": "pending", "directory_identities": {}, "dispositions": {}, "errors": []},
        "failure": None,
        "unresolved_limits": [
            "Build only: no publication, native install or provider acceptance is claimed."],
    }


def _require_disjoint_paths(paths: BuildPaths) -> None:
    """Refuse aliasing before any effect: the receipt and both trees stay disjoint, outside the source."""
    source = paths.source_root.resolve()
    work, output, receipt = (target.resolve() for target in (
        paths.work_dir, paths.output_dir, paths.receipt_path))
    if any(source == target or source in target.parents for target in (work, output, receipt)):
        raise VerificationError("build_path_inside_source", STAGE)
    if work == output or work in output.parents or output in work.parents:
        raise VerificationError("build_paths_overlap", STAGE)
    if any(tree == receipt or tree in receipt.parents for tree in (work, output)):
        raise VerificationError("build_receipt_inside_owned_tree", STAGE)


def _create_owned(owner: DirectoryOwner, receipt: dict, label: str, directory: Path) -> None:
    try:
        remember_directory(receipt, owner.create(label, directory, directory.parent))
    except (OSError, RuntimeError):
        raise VerificationError("build_directory_unavailable", STAGE) from None


def _require_exact_payload(context: BuildContext, manifest_bytes: bytes) -> None:
    names = tuple(sorted(child.name for child in context.output.iterdir()))
    if names != payload_paths(context.request.version):
        raise VerificationError("candidate_payload_not_exact", STAGE)
    written = _bounded_file_bytes(context.output / MANIFEST_PATH, MAX_SOURCE_FILE,
                                  "candidate_output_unreadable")
    manifest = parse_manifest(written)
    observed = observed_files(context)
    if written != manifest_bytes or any(
            observed[entry.path] != (entry.size, entry.sha256) for entry in manifest.artifacts):
        raise VerificationError("candidate_payload_changed", STAGE)


def _build(request: BuildRequest, paths: BuildPaths, context: BuildContext) -> CandidateManifest:
    tree = preflight_source(context.receipt, context.environment, request,
                            source_root=paths.source_root)
    _create_owned(context.owner, context.receipt, "candidate build clone", context.clone)
    clone_frozen(context.receipt, context.environment, request, source_root=paths.source_root,
                 clone=context.clone, tree=tree)
    require_package_version(
        _bounded_file_bytes(context.clone / "package.json", MAX_SOURCE_FILE, "package_json_unreadable"),
        request)
    inputs = observe_build_inputs(context, python=paths.python)
    build_release_files(context)
    collect_goreleaser_files(context)
    pack_npm(context)
    render_formula(context)
    verify_bundles(context)
    manifest = build_manifest(request, tree=tree, inputs=inputs, files=observed_files(context))
    manifest_bytes = encode_manifest(manifest)
    write_new(context.output / MANIFEST_PATH, manifest_bytes)
    _require_exact_payload(context, manifest_bytes)
    context.receipt.update({
        "tree": tree, "manifest_sha256": sha(manifest_bytes),
        "toolchains": {"go": inputs.toolchains.go, "goreleaser": inputs.toolchains.goreleaser,
                       "python": inputs.toolchains.python, "node": inputs.toolchains.node},
    })
    return manifest


def build_candidate(request: BuildRequest, paths: BuildPaths,
                    parent_environment: Mapping[str, str]) -> CandidateManifest:
    """Build the candidate, or raise leaving a settled receipt and no unproven deletion."""
    _require_disjoint_paths(paths)
    work = paths.work_dir.resolve()
    receipt = new_build_receipt(request)
    owners = BuildOwners(work=DirectoryOwner(), output=DirectoryOwner())
    context = BuildContext(
        request=request, receipt=receipt, environment=child_environment(parent_environment),
        clone=work / "clone", work=work, output=paths.output_dir.resolve(), owner=owners.work,
        goreleaser=paths.goreleaser)
    try:
        _create_owned(owners.work, receipt, "candidate build scratch", work)
        _create_owned(owners.output, receipt, "candidate payload", context.output)
        manifest = _build(request, paths, context)
    except BaseException as failure:
        settle(owners, receipt, paths.receipt_path, failure=failure)
        raise
    finalization_error = settle(owners, receipt, paths.receipt_path, failure=None)
    if finalization_error is not None:
        raise finalization_error
    return manifest
