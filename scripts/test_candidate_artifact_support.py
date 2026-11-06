"""Finite fixtures for the candidate artifact loader: real ZIP files, a fake ``gh``.

The fake ``gh`` is a real child found through PATH. It answers each endpoint from a
routes file in the test-owned tree, streams the container from a real file, and
appends one log line per invocation. Nothing touches the network or a provider.
"""

from __future__ import annotations

from collections.abc import Buffer
from dataclasses import dataclass
import io
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch
import zipfile

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification import candidate_artifact, github
from scripts.delegate_acceptance.release_verification.candidate_artifact import (
    CandidateArtifact, load_candidate_artifact,
)
from scripts.delegate_acceptance.release_verification.candidate_build_plan import (
    BuildRequest, build_inputs, build_manifest,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    CANDIDATE_WORKFLOW_PATH, MANIFEST_PATH, ArtifactKind, CandidateLocator, candidate_artifact_name,
    encode_manifest, expected_inventory,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_release_github_transport import ObservedGitHubClient


VERSION = "0.1.37"
COMMIT = "a1" * 20
OTHER_COMMIT = "e5" * 20
BASE = "c3" * 20
TREE = "b2" * 20
REPOSITORY = "hishamkaram/delegation-layer"
REPOSITORY_ID = 1368219645
WORKFLOW_ID = 357182906
RUN_ID = 39714439980
RUN_ATTEMPT = 2
ARTIFACT_ID = 12399736217
MIB = 1024 * 1024
ARCHIVE_PATHS = tuple(path for path, kind, _os, _arch in expected_inventory(VERSION)
                      if kind is ArtifactKind.ARCHIVE)

_FAKE_GH = """#!{python}
import json, os, sys
argv = sys.argv[1:]
endpoint = argv[7]
with open(os.environ["FIXTURE_LOG"], "a") as log:
    log.write(json.dumps({"endpoint": endpoint, "argv": argv}) + "\\n")
with open(os.environ["FIXTURE_ROUTES"]) as source:
    route = json.load(source).get(endpoint)
if route is None:
    sys.stderr.write("fixture: no route\\n")
    sys.exit(1)
if "file" in route:
    with open(route["file"], "rb") as source:
        while block := source.read(1 << 20):
            sys.stdout.buffer.write(block)
else:
    sys.stdout.buffer.write(json.dumps(route["json"]).encode())
"""


def payload_files(**overrides: bytes) -> dict[str, bytes]:
    """Small distinct bytes for each of the seven inventory files, with overrides."""
    files = {path: f"{path} fixture bytes\n".encode() for path, *_ in expected_inventory(VERSION)}
    files.update(overrides)
    return files


def manifest_bytes(files: dict[str, bytes], *, commit: str = COMMIT) -> bytes:
    """The canonical producer's manifest of exactly these files."""
    request = BuildRequest(version=VERSION, commit=commit, base_commit=BASE, repository=REPOSITORY)
    inputs = build_inputs(config_sha256="1" * 64,
                          go_sum_sha256="2" * 64, go="1.27.3", goreleaser="2.18.1",
                          python="3.12.13", node="22.11.0")
    observed = {path: (len(data), sha(data)) for path, data in files.items()}
    return encode_manifest(build_manifest(request, tree=TREE, inputs=inputs, files=observed))


class _ForwardOnly(io.BufferedIOBase):
    """A non-seekable sink, so ZipFile writes data descriptors as the producer does."""

    def __init__(self, raw: io.BufferedWriter):
        super().__init__()
        self._raw = raw

    def write(self, data: Buffer) -> int:
        return self._raw.write(data)

    def writable(self) -> bool:
        return True

    def tell(self) -> int:
        return self._raw.tell()

    def flush(self) -> None:
        self._raw.flush()


@dataclass(frozen=True)
class Member:
    """One ZIP member to write, with the header fields a test may vary."""

    name: str
    data: bytes
    compression: int = zipfile.ZIP_DEFLATED
    external_attr: int = 0o100644 << 16
    comment: bytes = b""


def members(files: dict[str, bytes], manifest: bytes, **compression: int) -> list[Member]:
    """The flat payload members in the producer's sorted order."""
    entries = {**files, MANIFEST_PATH: manifest}
    return [Member(name, entries[name], compression.get(name, zipfile.ZIP_DEFLATED))
            for name in sorted(entries)]


def write_zip(path: Path, entries: list[Member], *, comment: bytes = b"", central=None) -> bytes:
    """Write a real streamed ZIP; ``central`` may alter central-directory records before close."""
    with path.open("xb") as raw, _ForwardOnly(raw) as writer, zipfile.ZipFile(writer, "w") as bundle:
        bundle.comment = comment
        for entry in entries:
            info = zipfile.ZipInfo(entry.name, date_time=(2026, 1, 1, 0, 0, 0))
            info.compress_type, info.external_attr, info.comment = (
                entry.compression, entry.external_attr, entry.comment)
            with bundle.open(info, "w") as member:
                for start in range(0, len(entry.data), MIB):
                    member.write(entry.data[start:start + MIB])
        if central is not None:
            central(bundle.infolist())
    return path.read_bytes()


def locator(container: bytes, manifest: bytes, **changes) -> CandidateLocator:
    values = {"repository": REPOSITORY, "run_id": RUN_ID, "run_attempt": RUN_ATTEMPT,
              "artifact_id": ARTIFACT_ID, "artifact_archive_sha256": sha(container),
              "manifest_sha256": sha(manifest), **changes}
    return CandidateLocator(**values)


def metadata(size: int, digest: str, *, head_sha: str = COMMIT) -> dict[str, dict]:
    """GitHub replies for one exact candidate, keyed by reply kind."""
    return {
        "repository": {"id": REPOSITORY_ID, "full_name": REPOSITORY, "private": True},
        "workflow": {"id": WORKFLOW_ID, "name": "CI", "path": CANDIDATE_WORKFLOW_PATH, "state": "active"},
        "artifact": {
            "id": ARTIFACT_ID, "name": candidate_artifact_name(RUN_ID, RUN_ATTEMPT),
            "size_in_bytes": size, "digest": "sha256:" + digest, "expired": False,
            "workflow_run": {"id": RUN_ID, "repository_id": REPOSITORY_ID,
                             "head_repository_id": REPOSITORY_ID, "head_branch": "task",
                             "head_sha": head_sha}},
        "attempt": {
            "id": RUN_ID, "run_attempt": RUN_ATTEMPT, "workflow_id": WORKFLOW_ID, "head_sha": head_sha,
            "path": CANDIDATE_WORKFLOW_PATH, "status": "completed", "conclusion": "success",
            "repository": {"id": REPOSITORY_ID}, "head_repository": {"id": REPOSITORY_ID}},
    }


ENDPOINTS = {
    "repository": f"repos/{REPOSITORY}",
    "workflow": f"repos/{REPOSITORY}/actions/workflows/ci.yml",
    "artifact": f"repos/{REPOSITORY}/actions/artifacts/{ARTIFACT_ID}",
    "attempt": f"repos/{REPOSITORY}/actions/runs/{RUN_ID}",
}
DOWNLOAD = f"repos/{REPOSITORY}/actions/artifacts/{ARTIFACT_ID}/zip"
FULL_SEQUENCE = [*ENDPOINTS.values(), DOWNLOAD]


@dataclass(frozen=True)
class Served:
    """One container served by the fake ``gh`` and the locator a caller would trust."""

    container: Path
    locator: CandidateLocator
    files: dict[str, bytes]
    manifest: bytes


class ReadSpy:
    """The container file as the loader opened it, recording every forwarded read size."""

    def __init__(self, raw: io.BufferedReader, reads: list[int]):
        self._raw, self._reads = raw, reads

    def read(self, size: int = -1) -> bytes:
        self._reads.append(size)
        return self._raw.read(size)

    def __getattr__(self, name: str):
        return getattr(self._raw, name)

    def __enter__(self) -> ReadSpy:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self._raw.close()


class CandidateArtifactCase(unittest.TestCase):
    """A test-owned tree with a fake ``gh`` and fresh destinations for each load."""

    def setUp(self):
        descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), descriptors))
        self.tree = FixtureTree.open(prefix="candidate-artifact-test", label="candidate artifact fixture")
        self.addCleanup(self.tree.dispose)
        self.root = self.tree.root
        (self.root / "bin").mkdir()
        gh = self.root / "bin" / "gh"
        gh.write_text(_FAKE_GH.replace("{python}", sys.executable))
        gh.chmod(0o755)
        self.log_path, self.routes_path = self.root / "invocations.jsonl", self.root / "routes.json"
        self.launched: list[list[str]] = []
        self.loads = 0

    def serve(self, entries: list[Member] | None = None, *, files: dict[str, bytes] | None = None,
              manifest: bytes | None = None, change=None, **zip_options) -> Served:
        """Write the container, then route metadata, edited by ``change``, to it."""
        files = payload_files() if files is None else files
        manifest = manifest_bytes(files) if manifest is None else manifest
        container = self.root / f"served-{len(list(self.root.glob('served-*')))}.zip"
        data = write_zip(container, members(files, manifest) if entries is None else entries, **zip_options)
        replies = metadata(len(data), sha(data))
        if change is not None:
            change(replies)
        routes: dict[str, dict[str, object]] = {ENDPOINTS[kind]: {"json": reply} for kind, reply in replies.items()}
        routes[DOWNLOAD] = {"file": str(container)}
        self.routes_path.write_text(json.dumps(routes))
        return Served(container, locator(data, manifest), files, manifest)

    def replace_container(self, served: Served, data: bytes) -> Served:
        """Serve ``data`` instead, with metadata and a locator that describe it exactly."""
        served.container.unlink()
        served.container.write_bytes(data)
        routes = json.loads(self.routes_path.read_text())
        artifact = routes[ENDPOINTS["artifact"]]["json"]
        artifact.update(size_in_bytes=len(data), digest="sha256:" + sha(data))
        self.routes_path.write_text(json.dumps(routes))
        return Served(served.container, locator(data, served.manifest), served.files, served.manifest)

    def destinations(self) -> tuple[Path, Path]:
        self.loads += 1
        case = self.root / f"load-{self.loads}"
        (case / "payload").mkdir(parents=True)
        return case / "container.zip", case / "payload"

    def load(self, target: CandidateLocator, *, container: Path | None = None,
             payload: Path | None = None) -> CandidateArtifact:
        """Run the real loader through the canonical GitHub client and record every child."""
        if container is None or payload is None:
            container, payload = self.destinations()
        environment = {"PATH": str(self.root / "bin"), "FIXTURE_LOG": str(self.log_path),
                       "FIXTURE_ROUTES": str(self.routes_path), "GH_TOKEN": "private-canary"}
        client = ObservedGitHubClient({"commands": []}, environment, self.tree)
        real = github.run_bounded_command

        def launch(argv, *arguments, **options):
            self.launched.append(list(argv))
            return real(argv, *arguments, **options)

        with patch.object(github, "run_bounded_command", side_effect=launch):
            return load_candidate_artifact(client, target.artifact_id, repository=target.repository,
                                           container=container, payload=payload)

    def refused(self, code: str, target: CandidateLocator, **destinations: Path) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            self.load(target, **destinations)
        self.assertEqual(caught.exception.code, code)
        return caught.exception

    def requested(self) -> list[str]:
        if not self.log_path.exists():
            return []
        return [json.loads(line)["endpoint"] for line in self.log_path.read_text().splitlines()]

    def assert_only_github_children(self) -> None:
        self.assertTrue(self.launched)
        for argv in self.launched:
            self.assertEqual(argv[:2], ["gh", "api"])
            self.assertIn(argv[3], {"GET"})

    def payload_names(self) -> list[str]:
        return sorted(child.name for child in (self.root / f"load-{self.loads}" / "payload").iterdir())

    def spy_reads(self, reads: list[int]):
        """Record each read the loader forwards to its own opened container file."""
        def opened(path, mode="r", *, opener=None):
            return ReadSpy(open(path, mode, opener=opener), reads)

        return patch.object(candidate_artifact, "open", side_effect=opened, create=True)
