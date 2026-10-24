"""Fixtures for the maintainer record command: a real Git source, canonical records and the store case.

Every record comes from its canonical encoder and every proof ZIP from the
canonical candidate proof fixture, for a manifest that pins a real fixture
commit. Constants are test inputs, not evidence about the real repository, a
provider, an installation or a reviewer.
"""

from __future__ import annotations

import contextlib
from dataclasses import replace
import errno
import io
import json
import os
from pathlib import Path
import secrets
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.native_evidence import candidate_proof_fixture as proof_fixture
from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification import (
    candidate_record, candidate_record_receipt, candidate_store, github,
)
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    AttemptIntent, ChangeScope, PlatformClaim, SourceReview, candidate_proof_expectation, encode_claim,
    encode_intent, encode_review,
)
from scripts.delegate_acceptance.release_verification.candidate_build_source import head_tree
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    InstallChannel, SourceFileBinding, encode_install_proof, encode_install_witness,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    SourceIdentity, encode_locator, encode_manifest,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import BoundCandidate
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    EntryFamily, EntryName, TransitionKind, entry_name,
)
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.test_candidate_source_support import SourceRepository
from scripts.test_candidate_store_support import BRANCH, FakeGitHub, Store

DARWIN, LINUX = ("Darwin", "arm64"), ("Linux", "amd64")
PLANNED = (DARWIN, LINUX)
Files = dict[str, bytes]


def name(family: EntryFamily, **identity: object) -> str:
    return entry_name(EntryName(family, **identity))


class CandidateRecords:
    """Canonical records of one candidate whose manifest pins ``source``; claims bind the proof fixture."""

    def __init__(self, source: SourceIdentity):
        manifest = replace(fx.manifest(), source=source)
        self.candidate = BoundCandidate(locator=fx.locator_for(manifest), manifest=manifest)
        self.id = self.candidate.candidate_id
        self.source = source
        self.passing = proof_fixture.release_proof(failed=False)
        self.native = proof_fixture.expectation(self.passing)

    def register(self) -> Files:
        return {name(EntryFamily.LOCATOR): encode_locator(self.candidate.locator),
                name(EntryFamily.MANIFEST): encode_manifest(self.candidate.manifest)}

    def intent(self, attempt: int = 1, **changes: object) -> AttemptIntent:
        value = fx.intent(self.candidate, attempt=attempt, platforms=PLANNED,
                          triage_sha256=changes.pop("triage_sha256", None))
        return replace(value, **{"source_commit": self.source.commit, **changes})

    def intent_file(self, value: AttemptIntent) -> Files:
        return {name(EntryFamily.INTENT, attempt=value.attempt): encode_intent(value)}

    def claim(self, platform: tuple[str, str], attempt: int = 1) -> PlatformClaim:
        native = self.native
        return PlatformClaim(
            candidate_id=self.id, attempt=attempt, os=platform[0], arch=platform[1],
            invocation_id=fx.invocation_id(attempt, platform), harness_revision=native.harness_revision,
            harness_sha256=native.harness_sha256, binaries=native.binaries, opencode_model=native.opencode_model,
            ccr_model=native.ccr_model, agy_model=native.agy_model)

    def claim_file(self, claim: PlatformClaim) -> Files:
        return {name(EntryFamily.CLAIM, attempt=claim.attempt, platform=claim.platform): encode_claim(claim)}

    def proof_file(self, claim: PlatformClaim) -> Files:
        facts = candidate_proof_expectation(self.candidate.manifest, self.candidate.locator, claim)
        data = proof_fixture.candidate_zip(self.passing, facts)
        return {name(EntryFamily.PROOF, attempt=claim.attempt, platform=claim.platform): data}

    def install(self, platform: tuple[str, str], channel: InstallChannel) -> tuple[bytes, bytes]:
        """The canonical proof and witness bytes of one install of the native binaries."""
        binaries = self.native.binaries
        proof = fx.install(self.candidate, platform=platform, channel=channel, binaries=binaries)
        if isinstance(proof.installer, SourceFileBinding):
            proof = replace(proof, installer=replace(proof.installer, source_commit=self.source.commit))
        witness = fx.witness(self.candidate, platform=platform, channel=channel, binaries=binaries)
        return encode_install_proof(proof), encode_install_witness(witness)

    def install_file(self, platform: tuple[str, str], channel: InstallChannel) -> Files:
        proof, witness = self.install(platform, channel)
        role = {"platform": platform, "channel": channel}
        return {name(EntryFamily.INSTALL_PROOF, **role): proof, name(EntryFamily.INSTALL_WITNESS, **role): witness}

    def review(self, change: ChangeScope | None = None) -> SourceReview:
        return SourceReview(candidate_id=self.id, head_commit=self.source.commit, tree=self.source.tree,
                            reviewer=fx.REVIEWER, unresolved_findings=(),
                            reviewed_scope=fx.reviewed_scope(change, base=self.source.base_commit))

    def review_file(self, record: SourceReview) -> Files:
        data = encode_review(record)
        return {name(EntryFamily.REVIEW, digest=sha(data)): data}

    def ready(self) -> list[Files]:
        """The ready history before its freeze: registration, attempt 1, both proofs, every install, a review."""
        claims = [self.claim(platform) for platform in PLANNED]
        return [self.register(), self.intent_file(self.intent()), *(self.claim_file(item) for item in claims),
                *(self.proof_file(item) for item in claims),
                *(self.install_file(platform, channel) for platform in PLANNED for channel in InstallChannel),
                self.review_file(self.review())]


WRITE_METHODS = frozenset({"POST", "PUT", "PATCH", "DELETE"})
TOKEN = "fixture-token-" + "y" * 24
# Inherited by any Git child that leaked the caller environment, where it would break every read.
LEAKED_GIT = {"GIT_DIR": "/nonexistent/fixture-git-dir", "GIT_CONFIG_PARAMETERS": "'fixture.broken"}


def findings(decision) -> list[dict[str, str]]:
    """The receipt form of canonical findings."""
    return [{"code": item.code, "subject": item.subject} for item in decision.findings]


class RecordCase(unittest.TestCase):
    """One candidate whose manifest pins a real fixture commit; the class tree owns every child."""

    tree: FixtureTree
    source: SourceRepository
    moved: SourceRepository
    later: str
    records: CandidateRecords

    @classmethod
    def setUpClass(cls):
        cls.tree = FixtureTree.open(prefix="candidate-record", label="candidate record fixture")
        cls.addClassCleanup(cls.tree.dispose)
        cls.source = SourceRepository(cls.tree, "source")
        base = cls.source.commit({"keep.txt": b"keep\n"}, "base")
        head = cls.source.commit({"keep.txt": b"kept and changed\n"}, "head")
        # The same history checked out one commit further: its HEAD is no longer the manifest source.
        cls.moved = SourceRepository(cls.tree, "moved", clone_of=cls.source)
        cls.later = cls.moved.commit({"later.txt": b"later\n"}, "later")
        cls.records = CandidateRecords(SourceIdentity(commit=head, tree=cls.source.tree_of(head), base_commit=base))

    def setUp(self):
        self.github = FakeGitHub(repository=fx.REPOSITORY)
        self.store = Store(self.github)
        self.github.source(self.records.source.commit, {"install.sh": ("100755", self.github.blob(fx.INSTALL_SH)),
                                                         "README.md": ("100644", self.github.blob(b"readme\n"))})
        self.environments: list[dict[str, str]] = []
        self.on_patch = None
        self.after_observation = None
        transport = patch.object(github, "run_bounded_command", self.transport)
        transport.start()
        self.addCleanup(transport.stop)
        # Read at call time, so a test that owns a separate tree receives its own children's faults.
        observer = patch.object(candidate_record, "head_tree", self.observe_head)
        observer.start()
        self.addCleanup(observer.stop)
        self.work, self.scratch = self.owned("work"), self.owned("scratch")

    def owned(self, label: str) -> Path:
        """A new directory inside the fixture tree, which owns and disposes of it."""
        path = self.tree.root / f"{label}-{secrets.token_hex(6)}"
        path.mkdir()
        return path

    def transport(self, argv: list[str], environment: dict[str, str], **options: object):
        self.environments.append(dict(environment))
        if argv[3] == "PATCH" and self.on_patch is not None:
            hook, self.on_patch = self.on_patch, None
            hook()
        return self.github(argv, environment, **options)

    def observe_head(self, receipt: dict, environment: dict[str, str], *, repository: Path) -> tuple[str, str]:
        """The canonical observer; its original child failure reaches the tree before the command converts it."""
        observed = self.tree.observe_operation(lambda: head_tree(receipt, environment, repository=repository))
        if self.after_observation is not None:
            self.after_observation()
        return observed

    def seed(self, *groups: Files) -> str:
        """Commit each group as its own store transaction on the current head; return the new head."""
        for files in groups:
            for entry, data in files.items():
                self.store.put(f"candidates/{self.records.id}/{entry}", data)
            self.store.step("seed", parents=(self.github.refs[BRANCH],))
        return self.github.refs[BRANCH]

    def advance(self) -> str:
        """Another candidate's commit moves the branch; this candidate's own history is unchanged."""
        self.store.put(f"candidates/{'9' * 64}/locator.json", b"another candidate\n")
        return self.store.step("another candidate", parents=(self.github.refs[BRANCH],))

    def input(self, data: bytes) -> Path:
        path = self.scratch / f"input-{secrets.token_hex(4)}.json"
        path.write_bytes(data)
        return path

    def run_command(self, command: str, *arguments: str, receipt: Path | None = None) -> tuple[int, dict, str]:
        self.receipt = receipt or self.scratch / f"receipt-{secrets.token_hex(4)}.json"
        location = self.store.location
        argv = [command, *arguments, "--store-repository", location.repository, "--store-branch", location.branch,
                "--store-genesis-commit", location.genesis_commit, "--store-marker-blob", location.marker_blob,
                "--work-dir", str(self.work), "--receipt", str(self.receipt)]
        stdout, stderr = io.StringIO(), io.StringIO()
        leaked = {"GH_TOKEN": TOKEN, "GITHUB_TOKEN": TOKEN, **LEAKED_GIT}
        with patch.dict(os.environ, leaked), contextlib.redirect_stdout(stdout), \
                contextlib.redirect_stderr(stderr):
            code = candidate_record.main(argv)
        self.assertNotIn(TOKEN, stdout.getvalue() + stderr.getvalue())
        for environment in self.environments:
            self.assertFalse({"GH_TOKEN", "GITHUB_TOKEN"} & set(environment))
        return code, json.loads(stdout.getvalue()), stderr.getvalue()

    def register(self, locator: bytes, manifest: bytes):
        return self.run_command("register", "--locator", str(self.input(locator)),
                                "--manifest", str(self.input(manifest)))

    def intent(self, data: bytes):
        return self.run_command("intent", "--intent", str(self.input(data)))

    def triage(self, data: bytes):
        return self.run_command("triage", "--triage", str(self.input(data)))

    def install(self, proof: bytes, witness: bytes):
        return self.run_command("install", "--proof", str(self.input(proof)), "--witness", str(self.input(witness)))

    def freeze(self, review_sha256: str, *, repository: Path | None = None, candidate_id: str | None = None):
        return self.run_command("freeze", "--candidate-id", candidate_id or self.records.id,
                                "--review-sha256", review_sha256,
                                "--repository-root", str(repository or self.source.path))

    @contextlib.contextmanager
    def receipt_close_fault(self):
        """Fail only the reserved receipt's close, after really closing it; no other descriptor is touched.

        Yields every close the receipt owner attempted, so a test proves one close and no leak.
        """
        real_create, reserved, closed = candidate_record_receipt.create_new_file, [], []

        def create(path, code, *, stage):
            reserved.append(real_create(path, code, stage=stage))
            return reserved[-1]

        def close(descriptor):
            closed.append(descriptor)
            os.close(descriptor)
            if descriptor in reserved:
                raise OSError(errno.EIO, "fixture close fault")

        with patch.object(candidate_record_receipt, "create_new_file", create), \
                patch.object(candidate_record_receipt, "os", SimpleNamespace(fsync=os.fsync, close=close)):
            yield closed
        self.assertEqual(closed, reserved)
        with self.assertRaises(OSError):
            os.fstat(reserved[0])

    def document(self) -> dict:
        text = self.receipt.read_text()
        self.assertNotIn(TOKEN, text)
        return json.loads(text)

    def writes(self) -> list[tuple[str, str]]:
        return [call for call in self.github.calls if call[0] in WRITE_METHODS]

    def patches(self) -> int:
        return [method for method, _ in self.github.calls].count("PATCH")

    def calls_after_patch(self) -> list[tuple[str, str]]:
        index = next(i for i, (method, _) in enumerate(self.github.calls) if method == "PATCH")
        return self.github.calls[index + 1:]

    def git_commands(self) -> list[str]:
        return [item["name"] for item in self.document()["commands"] if item["name"].startswith("git-")]

    def readback(self):
        return candidate_store.load_candidate(GitHubClient({"commands": []}, {}), self.store.location,
                                              self.records.id, closure_resolutions=())

    def stored(self, commit: str, entry: str) -> bytes:
        files = self.github.flatten(self.github.commits[commit].tree)
        return self.github.blobs[files[f"candidates/{self.records.id}/{entry}"][1]]

    def assertConfirmed(self, result: tuple[int, dict, str], kind: TransitionKind, parent: str) -> dict:
        code, reported, _ = result
        self.assertEqual((code, reported["disposition"], reported["receipt_error"]), (0, "confirmed", None))
        self.assertEqual((reported["parent"], reported["kind"]), (parent, kind.value))
        self.assertEqual(self.github.refs[BRANCH], reported["commit"])
        self.assertEqual((self.patches(), self.document()["outcome"]["commit"]), (1, reported["commit"]))
        return reported

    def assertRefusedBeforeWrite(self, result: tuple[int, dict, str], code: str, head: str) -> dict:
        exit_code, reported, _ = result
        self.assertEqual((exit_code, reported["disposition"], reported.get("code")), (1, "refused", code))
        self.assertEqual((self.writes(), self.github.refs[BRANCH]), ([], head))
        self.assertEqual(list(self.work.iterdir()), [])
        self.assertEqual(self.document()["outcome"], {key: reported[key] for key in (
            "disposition", "code", "stage", "findings")})
        return reported
