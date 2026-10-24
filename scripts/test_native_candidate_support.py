"""A finite composed fixture for the candidate target of the native coordinator.

Real owners run end to end: the invocation receipt, the GitHub client, the
candidate store's public load and append with canonical replay, the candidate
artifact loader over a real ZIP and tar.gz, the in-process archive verifier,
the native process owner, native evidence staging and projection, and the
cleanup journal. Only these are fixtures:

* GitHub is the in-memory ``FakeGitHub`` behind the real client, extended with
  the four artifact metadata replies and a streamed artifact download.
* The executing harness is a fresh Git checkout holding byte-exact committed
  copies of the files the native-acceptance fingerprint hashes.
* The native child is one finite ``sh`` script: it writes a canonical fixture
  case matrix bound to the extracted binaries, then runs the real aggregate CLI
  with the call form of ``acceptance_native_all.sh``. No provider runs.

Nothing reaches the network, a provider or a real store.
"""

from __future__ import annotations

from collections.abc import Iterator
from contextlib import ExitStack, contextmanager
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import sys
import tarfile
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.native_release import candidate, cli, coordinator, execution
from scripts.delegate_acceptance.native_release.contract import (
    CandidateRequest, Invocation, invocation_name,
)
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.processes.contracts import BoundedCommandResult
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.release_verification import commands, github
from scripts.delegate_acceptance.release_verification.assets import platform_identity
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    AttemptIntent, PlatformClaim, claim_sha256, encode_claim, encode_intent,
)
from scripts.delegate_acceptance.release_verification.candidate_closure import (
    NATIVE_CHILD_RESOURCE, ClaimSubject, ResourceDisposition, encode_closure_evidence,
)
from scripts.delegate_acceptance.release_verification.candidate_history import encode_triage
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    PLATFORMS, ArtifactKind, CandidateLocator, bind_manifest, encode_locator, expected_inventory, parse_manifest,
)
from scripts.delegate_acceptance.release_verification.candidate_store import load_candidate
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    EntryFamily, EntryName, StoreLocation, candidate_path, entry_name,
)
from scripts.delegate_acceptance.release_verification.candidate_store_replay import CandidateSnapshot
from scripts.delegate_acceptance.release_verification.candidate_readiness import BoundCandidate
from scripts.delegate_acceptance.release_verification.contract import BINARIES
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.source import source_fingerprint
from scripts.test_candidate_artifact_support import (
    ARTIFACT_ID, ENDPOINTS, REPOSITORY, REPOSITORY_ID, VERSION, locator, manifest_bytes, members, metadata,
    payload_files, write_zip,
)
from scripts.test_candidate_store_support import FakeGitHub, Fault, Store


SOURCE_ROOT = Path(__file__).resolve().parent.parent
MODEL = "fixture-model"
DOWNLOAD_ROUTE = f"/actions/artifacts/{ARTIFACT_ID}/zip"
NATIVE_SCRIPT = """set -eu
python=$1 source=$2 helper=$3 mode=$4 note=$5
out=$DELEGATE_ACCEPTANCE_OUTPUT_ROOT
"$python" "$helper" "$source" "$out" "$mode" "$note"
if "$python" "$source/scripts/native_acceptance_evidence.py" aggregate \\
    --aggregate-root "$out" --matrix "$out/case-matrix.tsv" --receipt "$out/aggregate-receipt.json" \\
    --retention-count "${DELEGATE_ACCEPTANCE_RESOLVED_RETENTION:-3}" \\
    --tag "${DELEGATE_ACCEPTANCE_TAG:-}" \\
    --source-revision "${DELEGATE_ACCEPTANCE_SOURCE_REVISION:-}" >/dev/null; then
    exit 0
else
    exit $?
fi
"""
# The fixture matrix producer the finite child runs: canonical fixture cases bound
# to the binaries it finds where the coordinator extracted them.
MATRIX_HELPER = """import hashlib, json, os, sys
from pathlib import Path
source, output, mode, note = sys.argv[1:5]
sys.dont_write_bytecode = True
sys.path[:0] = [source, source + "/scripts"]
from scripts.test_native_acceptance_evidence import NativeAcceptanceEvidenceTests
from scripts.native_acceptance_evidence import EXPECTED_CASES
keys = ("TAG", "DELEGATE_ACCEPTANCE_TAG", "DELEGATE_ACCEPTANCE_SOURCE_REVISION", "OPENCODE_LIVE_MODEL")
Path(note).write_text(json.dumps({key: os.environ.get(key) for key in keys}))
status, failed, cleanup = {"pass": ("PASS", None, True), "fail": ("PASS", 1, True),
                           "blocked": ("BLOCKED", None, True), "unresolved": ("PASS", None, False)}[mode]
root = Path(output)
object.__new__(NativeAcceptanceEvidenceTests).make_matrix(
    root, status=status, failed_index=failed, with_cleanup=cleanup, requested_model="fixture-model")
binaries = Path(os.environ["DELEGATE_ACCEPTANCE_BIN_DIR"])
fields = {"delegate_sha256": "delegate", "runner_sha256": "delegate-run", "pueue_sha256": "pueue",
          "pueued_sha256": "pueued"}
hashes = {field: hashlib.sha256((binaries / name).read_bytes()).hexdigest() for field, name in fields.items()}
for case in EXPECTED_CASES:
    binding = root / "cases" / case.case_id / "binding.json"
    if binding.exists():
        value = json.loads(binding.read_bytes())
        value.update(hashes)
        binding.write_text(json.dumps(value))
"""


def git_environment(home: Path) -> dict[str, str]:
    """A minimal Git environment: no inherited credentials and no global or system configuration."""
    return {"PATH": os.environ["PATH"], "HOME": str(home), "LC_ALL": "C", "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"}


def git(tree: FixtureTree, checkout: Path, *arguments: str) -> bytes:
    """Run one harness Git child; ``tree`` observes its raw failure before ``command`` converts it."""
    env = git_environment(tree.root)
    argv = ["git", "-c", "user.name=Fixture Only", "-c", "user.email=fixture@example.invalid",
            "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false", *arguments]
    with observed_commands(tree):
        return commands.command({"commands": []}, "fixture-git", argv, env, cwd=checkout, capture=True,
                                timeout=60)


@contextmanager
def observed_commands(tree: FixtureTree, *, dependencies: tuple[FixtureTree, ...] = ()) -> Iterator[None]:
    """Run every release control child through ``tree``'s observer, before any caller converts its failure.

    ``dependencies`` are the other trees those children use as input; they observe the same
    original bounded-command failure.
    """
    run = commands.run_bounded_command

    def observed(*arguments: object, **options: object) -> BoundedCommandResult:
        return tree.observe_operation(lambda: run(*arguments, **options), dependencies=dependencies)

    with patch.object(commands, "run_bounded_command", observed):
        yield


def harness_checkout(tree: FixtureTree) -> Path:
    """A real Git checkout whose HEAD holds byte-exact copies of every fingerprinted harness file."""
    checkout = tree.root / "harness"
    for item in source_fingerprint("native-acceptance")["files"]:
        origin, target = SOURCE_ROOT / item["path"], checkout / item["path"]
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(origin, target)
        shutil.copymode(origin, target)
    git(tree, checkout, "init", "--quiet")
    git(tree, checkout, "add", "-A")
    git(tree, checkout, "commit", "--quiet", "--no-verify", "-m", "fixture harness")
    return checkout


def archive_bytes() -> bytes:
    """A deterministic tar.gz holding one small executable file per bundled binary name."""
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w:gz", format=tarfile.PAX_FORMAT) as bundle:
        for name in BINARIES:
            data = f"#!/bin/sh\n# candidate fixture binary {name}\nexit 0\n".encode()
            info = tarfile.TarInfo(name)
            info.size, info.mode, info.mtime = len(data), 0o755, 0
            bundle.addfile(info, io.BytesIO(data))
    return buffer.getvalue()


class CandidateGitHub(FakeGitHub):
    """The store's Git database plus one candidate's artifact metadata and streamed container."""

    def __init__(self) -> None:
        super().__init__(repository=REPOSITORY)
        self.repository_reply = {"id": REPOSITORY_ID, "full_name": REPOSITORY, "private": True}
        self.artifact_replies: dict[str, object] = {}
        self.container = b""
        self.downloads = 0

    def get(self, route: str) -> object:
        return self.artifact_replies.get(route, super().get(route))

    def __call__(self, argv: list[str], environment: dict[str, str], *, max_output: int,
                 **options: object) -> BoundedCommandResult:
        if argv[8] == f"repos/{self.repository}{DOWNLOAD_ROUTE}":
            self.calls.append((argv[3], argv[8]))
            self.downloads += 1
            sink = options["sink"]
            if not callable(sink):
                raise AssertionError("artifact download has no sink")
            for start in range(0, len(self.container), 1 << 16):
                sink(self.container[start:start + (1 << 16)])
            return BoundedCommandResult(0, b"")
        return super().__call__(argv, environment, max_output=max_output, **options)

    def writes(self, method: str) -> list[str]:
        return [endpoint for verb, endpoint in self.calls if verb == method]


class CandidateCase(unittest.TestCase):
    """One harness checkout per class; one invocation, store and artifact per test."""

    tree: FixtureTree
    checkout: Path
    native_script: Path
    matrix_helper: Path

    @classmethod
    def setUpClass(cls) -> None:
        cls.tree = FixtureTree.open(prefix="native-candidate-class", label="native candidate class fixture")
        cls.addClassCleanup(cls.tree.dispose)
        cls.checkout = harness_checkout(cls.tree)
        cls.native_script = cls.tree.root / "native.sh"
        cls.native_script.write_text(NATIVE_SCRIPT, encoding="utf-8")
        cls.matrix_helper = cls.tree.root / "matrix.py"
        cls.matrix_helper.write_text(MATRIX_HELPER, encoding="utf-8")

    def setUp(self) -> None:
        self.require_resolved("a test over the class harness")
        descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), descriptors))
        self.case = FixtureTree.open(prefix="native-candidate-case", label="native candidate case fixture")
        self.addCleanup(self.case.dispose)
        self.addCleanup(self.observe_native_closure)
        self.mode = "pass"
        self.note = self.case.root / "native-environment.json"
        self.starts = 0
        self.github = CandidateGitHub()
        self.store = Store(self.github)
        self.files = payload_files(**{self.archive_path(): archive_bytes()})
        self.manifest = manifest_bytes(self.files)
        container = write_zip(self.case.root / "container.zip", members(self.files, self.manifest))
        self.github.container = container
        self.github.artifact_replies = {
            endpoint.removeprefix(f"repos/{REPOSITORY}"): reply
            for kind, reply in metadata(len(container), hashlib.sha256(container).hexdigest()).items()
            if (endpoint := ENDPOINTS[kind]) != f"repos/{REPOSITORY}"}
        self.locator = locator(container, self.manifest)
        self.register(self.locator)
        self.put_record(EntryName(EntryFamily.INTENT, attempt=1), encode_intent(self.intent()), "intent")
        self.client = github.GitHubClient({"commands": []}, git_environment(self.case.root))
        self.receipt = self.create_receipt(self.request())

    def require_resolved(self, action: str) -> None:
        """Refuse ``action`` while a child that used the class harness has no proven closure."""
        if self.tree.unresolved is not None:
            raise RuntimeError(f"{action} refused: a child of {self.tree.root} has no proven closure")

    # Store and request ---------------------------------------------------------

    @staticmethod
    def platform() -> tuple[str, str]:
        identity = platform_identity()
        return identity["os"], identity["arch"]

    def archive_path(self) -> str:
        return next(path for path, kind, os_name, arch in expected_inventory(VERSION)
                    if kind is ArtifactKind.ARCHIVE and (os_name, arch) == self.platform())

    def intent(self) -> AttemptIntent:
        manifest = parse_manifest(self.manifest)
        return AttemptIntent(candidate_id=self.locator.candidate_id, attempt=1, source_commit=manifest.source.commit,
                             scope=fx.scope(), platforms=PLATFORMS, replaces=None)

    def put_record(self, name: EntryName, data: bytes, label: str) -> None:
        self.store.put(f"{candidate_path(self.locator.candidate_id)}/{entry_name(name)}", data)
        self.store.step(label)

    def register(self, value: CandidateLocator) -> None:
        directory = candidate_path(value.candidate_id)
        self.store.put(f"{directory}/{entry_name(EntryName(EntryFamily.LOCATOR))}", encode_locator(value))
        self.store.put(f"{directory}/{entry_name(EntryName(EntryFamily.MANIFEST))}", self.manifest)
        self.store.step("register")

    def request(self, value: CandidateLocator | None = None) -> CandidateRequest:
        return CandidateRequest(locator=value or self.locator, store=self.location(), attempt=1,
                                opencode_model=MODEL, ccr_model=MODEL, agy_model=MODEL)

    def location(self) -> StoreLocation:
        return self.store.location

    def foreign_claim(self) -> PlatformClaim:
        """Another invocation's claim of this attempt and platform."""
        os_name, arch = self.platform()
        return PlatformClaim(candidate_id=self.locator.candidate_id, attempt=1, os=os_name, arch=arch,
                             invocation_id="foreign-invocation", harness_revision="6" * 40,
                             harness_sha256="6" * 64, binaries=fx.NATIVE_BINARIES, opencode_model=MODEL,
                             ccr_model=MODEL, agy_model=MODEL)

    def put_claim(self, claim: PlatformClaim) -> None:
        self.put_record(EntryName(EntryFamily.CLAIM, attempt=claim.attempt, platform=claim.platform),
                        encode_claim(claim), "claim")

    def put_closure_triage(self, claim: PlatformClaim) -> None:
        """A triage whose closure fact only the native lifetime owner could supply."""
        fact = fx.resolution(ClaimSubject(claim_sha256(claim)), invocation=claim.invocation_id,
                             resources=((NATIVE_CHILD_RESOURCE, ResourceDisposition.WAITED),))
        bound = BoundCandidate(locator=self.locator, manifest=bind_manifest(self.locator, self.manifest))
        record = fx.triage(bound, attempt=1, prior_proofs=(), explained=(), prior_claims=(claim,),
                           closure_refs=(fx.reference(fact),)).record
        directory = candidate_path(self.locator.candidate_id)
        for family, data in ((EntryFamily.TRIAGE, encode_triage(record)),
                             (EntryFamily.CLOSURE, encode_closure_evidence(fact))):
            self.store.put(f"{directory}/{entry_name(EntryName(family, digest=sha(data)))}", data)
        self.store.step("triage")

    def create_receipt(self, request: CandidateRequest, invocation_id: str = "1" * 32) -> Receipt:
        root = self.case.root / invocation_name(request, invocation_id)
        owner = DirectoryOwner()
        try:
            token = owner.create("candidate test invocation", root, self.case.root)
            os_name, arch = self.platform()
            invocation = Invocation(invocation_id=invocation_id, request=request, checkout=self.checkout,
                                    root=root, platform_os=os_name, platform_arch=arch,
                                    directory_identity=directory_identity_record(token))
            return Receipt.create(invocation, ["run-candidate"])
        finally:
            owner.release()

    # Running -------------------------------------------------------------------

    def native_argv(self, invocation: Invocation) -> list[str]:
        return [str(Path("/bin/sh").resolve()), str(self.native_script), sys.executable, str(SOURCE_ROOT),
                str(self.matrix_helper), self.mode, str(self.note)]

    @contextmanager
    def owners(self) -> Iterator[None]:
        """Route GitHub to the fixture, count every native Start, and run the finite child.

        Every real control child reports its original failure to the case and the class
        harness it reads before the coordinator converts that failure into the receipt.
        """
        start = Processes.start

        def counted(processes: Processes, *arguments: object, **options: object):
            self.starts += 1
            return start(processes, *arguments, **options)

        with ExitStack() as stack:
            stack.enter_context(patch.object(github, "run_bounded_command", self.github))
            stack.enter_context(patch.object(coordinator, "github_client", return_value=self.client))
            stack.enter_context(patch.object(candidate, "native_argv", side_effect=self.native_argv))
            stack.enter_context(patch.object(Processes, "start", counted))
            stack.enter_context(observed_commands(self.case, dependencies=(self.tree,)))
            yield

    @contextmanager
    def fault_after_intent(self, stage: str, fault: Fault) -> Iterator[None]:
        """Arm one store fault once ``stage``'s intent is durable, so it hits that stage's append."""
        intend = Receipt.intend

        def armed(receipt: Receipt, name: str, intent: dict[str, object]) -> bool:
            fresh = intend(receipt, name, intent)
            if name == stage:
                self.github.faults.append(fault)
            return fresh

        with patch.object(Receipt, "intend", armed):
            yield

    @contextmanager
    def failing_completion(self, stage: str, error: BaseException) -> Iterator[None]:
        """Lose ``stage``'s result: the effect happened, its durable completion does not."""
        complete = Receipt.complete

        def failing(receipt: Receipt, name: str, result: dict[str, object]) -> None:
            if name == stage:
                raise error
            complete(receipt, name, result)

        with patch.object(Receipt, "complete", failing):
            yield

    def execute(self, receipt: Receipt | None = None) -> Receipt:
        """Run the public coordinator disposition over one receipt; return its durable state."""
        receipt = receipt or self.receipt
        with self.owners():
            cli._execute(receipt, recovery=False)
        return Receipt.load(receipt.path)

    def resume(self, receipt: Receipt | None = None) -> Receipt:
        """Reload the stored receipt and resume it, as the public resume command does."""
        loaded = Receipt.load((receipt or self.receipt).path)
        with self.owners():
            cli._execute(loaded, recovery=True)
        return Receipt.load(loaded.path)

    def observe_native_closure(self) -> None:
        """Retain the case and class trees unless every started native child's natural completion is proven.

        The child consumed the class script and helper as well as the case paths, so once
        a child started, any failure to load the receipt or observe its completion is an
        unproven closure that both trees observe, which then fails the test. A completed
        cleanup was authorized only after that completion was observed again.
        """
        if self.starts == 0:
            return

        def observe() -> None:
            receipt = Receipt.load(self.receipt.path)
            if receipt.stage("cleanup").state != "complete":
                execution.observe_child_completion(receipt, "native")

        self.case.observe_operation(observe, dependencies=(self.tree,), closure_required=True)

    def snapshot(self) -> CandidateSnapshot:
        with patch.object(github, "run_bounded_command", self.github):
            snapshot = load_candidate(self.client, self.location(), self.locator.candidate_id,
                                      closure_resolutions=())
        if snapshot is None:
            raise AssertionError("the fixture candidate is not registered")
        return snapshot

    def environment_note(self) -> dict[str, object]:
        value = json.loads(self.note.read_text(encoding="utf-8"))
        if not isinstance(value, dict):
            raise AssertionError("native environment note is not an object")
        return value
