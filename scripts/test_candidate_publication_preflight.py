"""Publication preflight over the real readiness producer's output behind the real ``GitHubClient``.

The canonical readiness adapter runs once per test against the in-memory store
and writes its actual three files; they are archived the way a single-directory
upload encodes them (DEFLATE with data descriptors). A finite Actions front
answers only the workflow, attempt, artifact listing and archive reads and
forwards every other request to the store. This is development (codec) proof:
it says nothing about a hosted artifact, which stays unproven until the
workflow runs from main.
"""

from __future__ import annotations

import copy
from dataclasses import FrozenInstanceError, dataclass, field, replace
import hashlib
import io
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch
import zipfile

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.native_release.contract import PublicationRequest
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, BoundedCommandResult, CommandFailure
from scripts.delegate_acceptance.release_verification import candidate_publication_preflight as preflight
from scripts.delegate_acceptance.release_verification import candidate_readiness_check as check
from scripts.delegate_acceptance.release_verification import candidate_store, github
from scripts.delegate_acceptance.release_verification.candidate_history import FindingRef
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import CandidateReference
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_readiness_check import HARNESS, HARNESS_SHA256, PULL_REQUEST, RUN_ID, ReadinessCase
from scripts.test_candidate_store_support import NOT_FOUND, RULESET_ID


ARTIFACT_ID = 4807001
WORKFLOW_ID = 2210
REPOSITORY_ID = 42  # the store fixture's repository identity
ACTIONS = f"repos/{fx.REPOSITORY}/actions/"
ARTIFACT_NAME = f"development-readiness-{RUN_ID}-1"
MEMBERS = tuple(preflight.MEMBER_LIMITS)
DELETE = object()


class _Unseekable(io.RawIOBase):
    """A write-only stream: ``zipfile`` then writes data descriptors (flag bit 3), as the upload action does."""

    def __init__(self) -> None:
        super().__init__()
        self.data = bytearray()

    def writable(self) -> bool:
        return True

    def write(self, chunk: bytes) -> int:
        self.data += chunk
        return len(chunk)


def upload_zip(members: dict[str, bytes], *, mode: int = 0o100600,
               compression: int = zipfile.ZIP_DEFLATED) -> bytes:
    stream = _Unseekable()
    with zipfile.ZipFile(stream, "w") as archive:
        for name, data in members.items():
            info = zipfile.ZipInfo(name, date_time=(2026, 10, 22, 0, 0, 0))
            info.compress_type, info.create_system, info.external_attr = compression, 3, mode << 16
            archive.writestr(info, data)
    return bytes(stream.data)


def with_change(document: dict, path: tuple[str, ...], value: object) -> dict:
    changed = copy.deepcopy(document)
    parent = changed
    for key in path[:-1]:
        parent = parent[key]
    if value is DELETE:
        del parent[path[-1]]
    else:
        parent[path[-1]] = value
    return changed


@dataclass
class ActionsFront:
    """Answer the readiness workflow, attempt, artifact listing and archive; forward everything else."""

    forward: object
    calls: list[tuple[str, str]]
    archive: bytes = b""
    attempt: dict[str, object] = field(default_factory=dict)
    artifact: dict[str, object] = field(default_factory=dict)
    workflow: dict[str, object] = field(default_factory=dict)
    listing: list[object] | None = None
    download: BoundedCommandResult | BaseException | None = None

    def row(self) -> dict[str, object]:
        return {"id": ARTIFACT_ID, "name": ARTIFACT_NAME, "size_in_bytes": len(self.archive),
                "digest": "sha256:" + hashlib.sha256(self.archive).hexdigest(), "expired": False,
                "workflow_run": {"id": int(RUN_ID), "repository_id": REPOSITORY_ID,
                                 "head_repository_id": REPOSITORY_ID, "head_branch": "main", "head_sha": HARNESS},
                **self.artifact}

    def reply(self, route: str) -> object:
        if route == "workflows/development-readiness.yml":
            return {"id": WORKFLOW_ID, "path": check.WORKFLOW_PATH, "state": "active", **self.workflow}
        if route == f"runs/{RUN_ID}/attempts/1":
            identity = {"id": REPOSITORY_ID, "full_name": fx.REPOSITORY}
            return {"id": int(RUN_ID), "run_attempt": 1, "workflow_id": WORKFLOW_ID, "path": check.WORKFLOW_PATH,
                    "head_branch": "main", "head_sha": HARNESS, "event": "workflow_dispatch",
                    "status": "completed", "conclusion": "success", "repository": identity,
                    "head_repository": identity, **self.attempt}
        if route.startswith(f"runs/{RUN_ID}/artifacts?per_page=100&page="):
            pages = self.listing or [{"total_count": 2, "artifacts": [
                self.row(), {**self.row(), "id": ARTIFACT_ID + 1, "name": "unrelated"}]}]
            page = int(route.rsplit("=", 1)[1])
            return pages[page - 1] if page <= len(pages) else None
        if route == f"artifacts/{ARTIFACT_ID}/zip":
            if isinstance(self.download, BaseException):
                raise self.download
            return self.download or self.archive
        return None

    def __call__(self, argv: list[str], environment: dict[str, str], *, max_output: int,
                 **options: object) -> BoundedCommandResult:
        method, endpoint = argv[3], argv[8]
        if method != "GET" or not endpoint.startswith(ACTIONS):
            return self.forward(argv, environment, max_output=max_output, **options)
        self.calls.append((method, endpoint))
        value = self.reply(endpoint[len(ACTIONS):])
        if isinstance(value, BoundedCommandResult):
            return value
        if value is None:
            return NOT_FOUND
        data = value if isinstance(value, bytes) else json.dumps(value).encode()
        if len(data) > max_output:
            raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED, 0)
        return BoundedCommandResult(0, data)


class PreflightCase(ReadinessCase):
    def setUp(self):
        super().setUp()
        self.actions = ActionsFront(self.front, self.github.calls)
        transport = patch.object(github, "run_bounded_command", self.actions)
        transport.start()
        self.addCleanup(transport.stop)

    def produce(self) -> str:
        """Freeze the ready history, run the real producer once and archive its output; return F."""
        freeze = self.freeze()
        code, summary, receipt, output = self.run_check(freeze)
        self.assertEqual((code, summary["receipt_finalized"]), (check.EXIT_READY, True))
        self.assertNotIn("receipt_finalized", receipt, "the producer only prints finalization")
        self.files = {name: (output / name).read_bytes() for name in MEMBERS}
        self.receipt = receipt
        self.actions.archive = upload_zip(self.files)
        return freeze

    def archive(self, *, receipt: dict | None = None, **members: bytes) -> None:
        files = {**self.files, **members}
        if receipt is not None:
            files[preflight.RECEIPT_MEMBER] = json.dumps(receipt).encode()
        self.actions.archive = upload_zip(files)

    def request(self, freeze: str, **changes: object) -> PublicationRequest:
        reference = CandidateReference(candidate_id=self.candidate.candidate_id, freeze_commit=freeze,
                                       merged_commit="6" * 40)
        values = {"store": self.store.location, "reference": reference, "readiness_run_id": int(RUN_ID),
                  "readiness_run_attempt": 1, "resume_published": False, **changes}
        return PublicationRequest(**values)

    def run_preflight(self, freeze: str, *, strict: bool = True, **changes: object) -> preflight.PublicationContext:
        self.github.calls.clear()
        return preflight.preflight(self.client, self.request(freeze, **changes), verify_store_protection=strict)

    def assertRefused(self, freeze: str, code: str, *, strict: bool = True, **changes: object) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            self.run_preflight(freeze, strict=strict, **changes)
        self.assertEqual(caught.exception.code, code)
        self.assertEqual(self.requests("POST") + self.requests("PATCH"), [], "a refusal precedes any mutation")
        return caught.exception

    def downloads(self) -> list[str]:
        return self.requests("GET", "/zip")


class ReadyPreflightTests(PreflightCase):
    def test_the_producer_output_preflights_without_a_persisted_finalization_field(self):
        freeze = self.produce()
        with zipfile.ZipFile(io.BytesIO(self.actions.archive)) as archive:
            self.assertEqual({(info.compress_type, info.flag_bits & 0x08) for info in archive.infolist()},
                             {(zipfile.ZIP_DEFLATED, 0x08)})
        context = self.run_preflight(freeze)
        self.assertEqual((context.pull_request, context.target),
                         (PULL_REQUEST, check.Target(fx.COMMIT, self.candidate.manifest.source.tree)))
        self.assertEqual(context.artifact, preflight.ReadinessArtifact(
            ARTIFACT_ID, hashlib.sha256(self.actions.archive).hexdigest()))
        self.assertEqual((context.harness_commit, context.harness_sha256), (HARNESS, HARNESS_SHA256))
        self.assertNotEqual(context.harness_commit, context.target.commit, "trusted main is not candidate B")
        self.assertEqual((context.snapshot.freeze_commit, context.snapshot.head), (freeze, freeze))
        self.assertEqual(context.review.sha256, self.receipt["selected_review_sha256"])
        self.assertIs(context.protection_unverified, False)
        self.assertIs(context.repository_private, True, "readiness and preflight may run privately")
        self.assertEqual(len(self.downloads()), 1)
        self.assertEqual(len(self.requests("GET", f"/rulesets/{RULESET_ID}")), 1, "the strict reader ran")
        self.assertEqual(self.requests("GET", "/pulls/"), [], "the receipt, not a new read, supplies N")
        order = [endpoint for _, endpoint in self.github.calls]
        self.assertLess(order.index(f"{ACTIONS}runs/{RUN_ID}/attempts/1"),
                        order.index(f"{ACTIONS}runs/{RUN_ID}/artifacts?per_page=100&page=1"))
        self.assertLess(order.index(f"{ACTIONS}artifacts/{ARTIFACT_ID}/zip"),
                        order.index(f"repos/{fx.REPOSITORY}/git/ref/heads/{self.github.branch}"))
        with self.assertRaises(FrozenInstanceError):
            setattr(context, "pull_request", 1)

    def test_hosted_reads_the_branch_head_and_keeps_protection_unverified(self):
        freeze = self.produce()
        context = self.run_preflight(freeze, strict=False)
        self.assertIs(context.protection_unverified, True)
        self.assertEqual(self.requests("GET", "/rules"), [])

    def test_a_later_unrelated_store_head_is_accepted(self):
        freeze = self.produce()
        tree = self.github.commits[freeze].tree
        later = self.github.commit(tree, (freeze,), "unrelated later append")
        self.github.refs[self.github.branch] = later
        for strict in (True, False):
            with self.subTest(strict=strict):
                context = self.run_preflight(freeze, strict=strict)
                self.assertEqual((context.snapshot.head, context.snapshot.freeze_commit), (later, freeze))

    def test_a_strict_store_refusal_never_falls_back_to_the_unprotected_reader(self):
        freeze = self.produce()
        self.github.rulesets[RULESET_ID]["bypass_actors"] = [{"actor_id": 1, "actor_type": "RepositoryRole"}]
        with self.assertRaises(VerificationError) as caught:
            self.run_preflight(freeze)
        self.assertEqual(caught.exception.stage, candidate_store.STAGE)
        self.assertEqual(len(self.requests("GET", "/git/ref/heads/")), 1, "the branch is read once, never again")
        self.assertEqual(self.requests("GET", "/contents/") + self.requests("GET", "/commits?"), [])
        self.assertIs(self.run_preflight(freeze, strict=False).protection_unverified, True)


class AttemptTests(PreflightCase):
    def test_a_failed_attempt_with_a_ready_looking_artifact_is_refused_before_artifact_use(self):
        freeze = self.produce()
        for status, conclusion in (("completed", "failure"), ("completed", "cancelled"), ("in_progress", None),
                                   ("completed", None)):
            with self.subTest(status=status, conclusion=conclusion):
                self.actions.attempt = {"status": status, "conclusion": conclusion}
                self.assertRefused(freeze, "readiness_attempt_not_successful")
                self.assertEqual(self.requests("GET", "/artifacts"), [])
                self.assertEqual(self.requests("GET", "/git/"), [])

    def test_the_exact_attempt_must_be_this_workflow_on_main_in_this_repository(self):
        freeze = self.produce()
        foreign = {"id": REPOSITORY_ID + 1, "full_name": fx.REPOSITORY}
        bare = check.WORKFLOW_PATH
        paths = ({"path": f"{bare}@other"}, {"path": f"{bare}@refs/heads/main"}, {"path": f"{bare}@main/x"},
                 {"path": f"{bare}@main@main"}, {"path": f"{bare}@main "}, {"path": f" {bare}"},
                 {"path": ".github/workflows/other.yml"}, {"path": ".github/workflows/other.yml@main"},
                 {"path": [bare]}, {"path": None})
        for change in ({"id": int(RUN_ID) + 1}, {"run_attempt": 2}, {"run_attempt": True},
                       {"workflow_id": WORKFLOW_ID + 1}, *paths,
                       {"head_branch": "task"}, {"event": "push"}, {"head_sha": "G" * 40},
                       {"repository": foreign}, {"head_repository": foreign}, {"repository": None}):
            with self.subTest(change=change):
                self.actions.attempt = change
                self.assertRefused(freeze, "readiness_attempt_mismatch")
        self.actions.attempt = {}
        for path in (".github/workflows/other.yml", f"{bare}@main"):
            with self.subTest(descriptor=path):
                self.actions.workflow = {"path": path}
                self.assertRefused(freeze, "readiness_workflow_mismatch")
        self.actions.workflow = {}
        self.github.repository_reply = {**self.github.repository_reply, "full_name": "other/repo"}
        self.assertRefused(freeze, "readiness_repository_mismatch")

    def test_a_main_qualified_attempt_path_is_the_same_trusted_workflow(self):
        freeze = self.produce()
        self.assertEqual(preflight.ATTEMPT_PATHS, (check.WORKFLOW_PATH, f"{check.WORKFLOW_PATH}@main"))
        self.actions.attempt = {"path": f"{check.WORKFLOW_PATH}@main"}
        context = self.run_preflight(freeze)
        self.assertEqual((context.harness_commit, context.pull_request), (HARNESS, PULL_REQUEST))
        self.assertEqual(len(self.downloads()), 1)

    def test_repository_privacy_is_carried_as_the_exact_observed_bool(self):
        freeze = self.produce()
        repository = f"repos/{fx.REPOSITORY}"
        for private in (True, False):
            with self.subTest(private=private):
                self.github.repository_reply = {**self.github.repository_reply, "private": private}
                context = self.run_preflight(freeze)
                self.assertIs(context.repository_private, private)
                order = list(self.github.calls)
                workflow = order.index(("GET", f"{ACTIONS}workflows/{preflight.WORKFLOW_FILE}"))
                self.assertEqual(order[:workflow].count(("GET", repository)), 1, "one repository read")

    def test_unknown_repository_privacy_refuses_before_any_later_read(self):
        freeze = self.produce()
        known = {key: value for key, value in self.github.repository_reply.items() if key != "private"}
        for label, reply in (("missing", known), *((repr(value), {**known, "private": value})
                                                   for value in (None, "true", "false", 0, 1, [], {}))):
            with self.subTest(private=label):
                self.github.repository_reply = reply
                self.assertRefused(freeze, "repository_visibility_unknown")
                self.assertEqual(self.github.calls, [("GET", f"repos/{fx.REPOSITORY}")])

    def test_inputs_are_checked_before_any_read(self):
        freeze = self.produce()
        for client, request, strict in ((object(), self.request(freeze), True), (self.client, object(), True),
                                        (self.client, self.request(freeze), 1),
                                        (self.client, self.request(freeze), None)):
            with self.subTest(strict=strict), self.assertRaises(VerificationError) as caught:
                self.github.calls.clear()
                preflight.preflight(client, request, verify_store_protection=strict)
            self.assertEqual(caught.exception.code, "publication_preflight_request_invalid")
            self.assertEqual(self.github.calls, [])


class ArtifactTests(PreflightCase):
    def test_exactly_one_unexpired_bound_artifact_of_the_attempt_is_selected(self):
        freeze = self.produce()
        cases = [
            ({"name": f"development-readiness-{RUN_ID}-2"}, "readiness_artifact_missing"),
            ({"expired": True}, "readiness_artifact_expired"),
            ({"expired": None}, "readiness_artifact_invalid"),
            ({"size_in_bytes": 0}, "readiness_artifact_invalid"),
            ({"size_in_bytes": True}, "readiness_artifact_invalid"),
            ({"size_in_bytes": preflight.MAX_ARCHIVE_BYTES + 1}, "readiness_artifact_too_large"),
            ({"digest": None}, "readiness_artifact_invalid"),
            ({"digest": hashlib.sha256(self.actions.archive).hexdigest()}, "readiness_artifact_invalid"),
        ]
        run = self.actions.row()["workflow_run"]
        for key, value in (("id", int(RUN_ID) + 1), ("head_sha", fx.COMMIT), ("repository_id", 9),
                           ("head_repository_id", 9)):
            cases.append(({"workflow_run": {**run, key: value}}, "readiness_artifact_mismatch"))
        for change, code in cases:
            with self.subTest(change=change):
                self.actions.artifact = change
                self.assertRefused(freeze, code)
                self.assertEqual(self.downloads(), [])
        self.actions.artifact = {}
        self.actions.listing = [{"total_count": 2, "artifacts": [self.actions.row(),
                                                                {**self.actions.row(), "id": ARTIFACT_ID + 1}]}]
        self.assertRefused(freeze, "readiness_artifact_ambiguous")

    def test_the_listing_must_be_complete_and_consistent(self):
        freeze = self.produce()
        row = self.actions.row()
        def page(number: int, total: int) -> dict[str, object]:
            return {"total_count": total, "artifacts": [{**row, "id": ARTIFACT_ID + number * 100 + index,
                                                         "name": "other"} for index in range(1, 101)]}

        full = page(1, 101)["artifacts"]
        cases = {
            "readiness_artifact_listing_incomplete": [
                [{"total_count": 2, "artifacts": [row]}],
                [page(1, 50)],
            ],
            "readiness_artifact_listing_invalid": [
                [{"total_count": 2, "artifacts": [row, row]}],
                [page(1, 1001)],
                [{"total_count": True, "artifacts": [row]}],
                [{"total_count": 101, "artifacts": full}, {"total_count": 102, "artifacts": [row]}],
                [{"total_count": 1, "artifacts": None}],
                [{"total_count": 1, "artifacts": [{**row, "id": "1"}]}],
            ],
        }
        for code, listings in cases.items():
            for listing in listings:
                with self.subTest(code=code, pages=len(listing)):
                    self.actions.listing = listing
                    self.assertRefused(freeze, code)
                    self.assertEqual(self.downloads(), [])
        self.actions.listing = [{"total_count": 101, "artifacts": full}, {"total_count": 101, "artifacts": [row]}]
        self.assertEqual(self.run_preflight(freeze).artifact.id, ARTIFACT_ID)

    def test_one_download_must_match_the_listed_compressed_size_and_digest(self):
        freeze = self.produce()
        archive = self.actions.archive
        with zipfile.ZipFile(io.BytesIO(archive)) as bundle:
            self.assertGreater(sum(info.file_size for info in bundle.infolist()), len(archive))
        for change in ({"size_in_bytes": len(archive) + 1},
                       {"digest": "sha256:" + hashlib.sha256(archive + b"x").hexdigest()}):
            with self.subTest(change=change):
                self.actions.artifact = change
                self.assertRefused(freeze, "readiness_archive_mismatch")
                self.assertEqual(len(self.downloads()), 1, "no retry")

    def test_transport_failures_keep_their_canonical_identity_and_chain(self):
        freeze = self.produce()
        self.actions.download = NOT_FOUND
        self.assertRefused(freeze, "github_request_failed")
        self.assertEqual(len(self.downloads()), 1)
        unknown = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)
        self.actions.download = unknown
        error = self.assertRefused(freeze, "command_termination_timeout")
        self.assertIs(error.__cause__, unknown)
        self.assertIsNone(unknown.exit_code, "Wait stays unknown")


class ArchiveTests(PreflightCase):
    def test_only_the_three_plain_producer_members_are_accepted(self):
        freeze = self.produce()
        receipt = self.files[preflight.RECEIPT_MEMBER]
        diagnostic = {f"_temp/readiness-status-token-probe/{name}": data for name, data in self.files.items()}
        diagnostic["delegation-layer/delegation-layer/.local/source-manifest.json"] = b"{}"
        cases = [
            (diagnostic, {}, "readiness_zip_inventory_mismatch"),
            ({**self.files, "extra.json": b"{}"}, {}, "readiness_zip_inventory_mismatch"),
            ({name: self.files[name] for name in MEMBERS[:2]}, {}, "readiness_zip_inventory_mismatch"),
            ({**self.files, "receipt.json/": b""}, {}, "readiness_zip_inventory_mismatch"),
            (self.files, {"mode": 0o120777}, "readiness_zip_member_invalid"),
            (self.files, {"mode": 0o040755}, "readiness_zip_member_invalid"),
            (self.files, {"compression": zipfile.ZIP_BZIP2}, "readiness_zip_member_invalid"),
            ({**self.files, "status-pending-body.json": b" " * (preflight.MAX_STATUS_BODY_BYTES + 1)}, {},
             "readiness_zip_member_too_large"),
        ]
        for members, options, code in cases:
            with self.subTest(code=code, names=sorted(members)[:1], options=options):
                self.actions.archive = upload_zip(members, **options)
                self.assertRefused(freeze, code)
                self.assertEqual(self.requests("GET", "/git/"), [], "no store read before the receipt binds")
        corrupt = upload_zip(self.files, compression=zipfile.ZIP_STORED).replace(receipt[:64], bytes(64), 1)
        for data in (b"PK\x03\x04 not a zip", corrupt):
            with self.subTest(size=len(data)):
                self.actions.archive = data
                self.assertRefused(freeze, "readiness_zip_invalid")

    def test_an_encrypted_member_is_refused(self):
        freeze = self.produce()
        data = bytearray(upload_zip(self.files, compression=zipfile.ZIP_STORED))
        position = data.index(b"PK\x01\x02")
        data[position + 8] |= 0x01  # the central directory's general-purpose flags
        self.actions.archive = bytes(data)
        self.assertRefused(freeze, "readiness_zip_member_invalid")

    def test_members_are_parsed_by_the_canonical_strict_parser(self):
        freeze = self.produce()
        cases = ((b'{"kind": 1, "kind": 2}', "public_json_duplicate_key"),
                 (b'{"value": NaN}', "readiness_member_json_invalid"),
                 (b'{"text": "\xff"}', "readiness_member_json_invalid"),
                 (b"[]", "readiness_member_json_invalid"))
        for data, code in cases:
            with self.subTest(code=code, data=data):
                self.archive(**{"receipt.json": data})
                error = self.assertRefused(freeze, code)
                self.assertNotIn(data.decode("latin-1"), str(error))


class ReceiptBindingTests(PreflightCase):
    def test_every_receipt_binding_refuses_independently(self):
        freeze = self.produce()
        other = "7" * 40
        cases = {
            "readiness_receipt_not_ready": [
                (("result",), "incomplete"), (("exit_code",), False), (("exit_code",), "0"), (("kind",), "other"),
                (("schema_version",), True), (("decision", "state"), "failure"),
                (("readiness", "ready"), 1), (("readiness", "findings"), [{"code": "x", "subject": "y"}]),
                (("protection_unverified",), False), (("status_context",), "other"),
            ],
            "readiness_run_mismatch": [
                (("run", "GITHUB_RUN_ATTEMPT"), "2"), (("run", "GITHUB_RUN_ID"), "5550002"),
                (("run", "GITHUB_SHA"), other), (("run", "GITHUB_REF"), "refs/heads/task"),
                (("run", "GITHUB_WORKFLOW_REF"), "other"), (("run", "GITHUB_REPOSITORY"), "other/repo"),
                (("run", "GITHUB_API_URL"), None), (("run", "GITHUB_EVENT_NAME"), DELETE),
            ],
            "readiness_harness_mismatch": [
                (("harness", "committed"), False), (("harness", "commit"), fx.COMMIT),
                (("harness", "sha256"), "E" * 64),
            ],
            "readiness_request_mismatch": [
                (("request", "candidate_id"), "1" * 64), (("request", "freeze_commit"), other),
                *((("request", "store", pin), "x") for pin in ("repository", "branch", "genesis_commit",
                                                                 "marker_blob")),
                (("store", "candidate_id"), "1" * 64), (("store", "freeze_commit"), other),
            ],
            "readiness_target_mismatch": [
                (("pull_request", "head_sha"), other), (("target", "tree"), "T" * 40),
                (("target", "commit"), other),
            ],
            "readiness_status_mismatch": [
                (("statuses", "pending", "delivery"), "unknown"), (("statuses", "terminal", "state"), "failure"),
                (("statuses", "terminal", "id"), self.receipt["statuses"]["pending"]["id"]),
                (("statuses", "pending", "id"), True), (("statuses", "terminal", "commit"), other),
                (("statuses", "pending", "target_url"), "https://github.com/other"),
                (("statuses", "terminal", "ack_mismatches"), ["id"]), (("statuses", "pending"), DELETE),
            ],
            "readiness_pull_request_mismatch": [
                (("request", "pull_request"), PULL_REQUEST + 1), (("pull_request", "number"), PULL_REQUEST + 1),
                (("pull_request", "number"), str(PULL_REQUEST)), (("request", "pull_request"), 0),
            ],
        }
        for code, changes in cases.items():
            for path, value in changes:
                with self.subTest(code=code, path=path, value=value):
                    self.archive(receipt=with_change(self.receipt, path, value))
                    self.assertRefused(freeze, code)
                    self.assertEqual(self.requests("GET", "/git/"), [], "the store is read only after binding")

    def test_the_request_must_name_the_receipt_candidate_and_store(self):
        freeze = self.produce()
        other = replace(self.store.location, genesis_commit="8" * 40)
        self.assertRefused(freeze, "readiness_request_mismatch", store=other)
        self.assertRefused(self.github.commits[freeze].parents[0], "readiness_request_mismatch")

    def test_archived_status_bodies_must_be_the_acknowledged_requests(self):
        freeze = self.produce()
        sent = json.loads(self.files["status-terminal-body.json"])
        for name, body in (("status-terminal-body.json", {**sent, "state": "failure"}),
                           ("status-pending-body.json", sent),
                           ("status-pending-body.json", {**sent, "state": "pending", "extra": 1})):
            with self.subTest(name=name, body=body):
                self.archive(**{name: json.dumps(body).encode()})
                self.assertRefused(freeze, "readiness_status_body_mismatch")


class FreshStoreTests(PreflightCase):
    def test_the_fresh_store_must_match_the_receipt(self):
        freeze = self.produce()
        source = self.receipt["store"]["candidate_source"]
        changes = [(("store", "publication_sha256"), "c" * 64),
                   (("store", "candidate_source"), {**source, "base_commit": "5" * 40}),
                   (("scope", "changed_providers"), ["codex"])]
        review = with_change(with_change(self.receipt, ("selected_review_sha256",), "d" * 64),
                             ("readiness", "review_sha256"), "d" * 64)
        for path, value in changes:
            with self.subTest(path=path):
                self.archive(receipt=with_change(self.receipt, path, value))
                self.assertRefused(freeze, "publication_snapshot_mismatch")
        self.archive(receipt=review)
        self.assertRefused(freeze, "publication_snapshot_mismatch")

    def test_an_absent_or_differently_frozen_fresh_candidate_is_refused(self):
        freeze = self.produce()
        snapshot = self.loaded()
        for value, code in ((None, "publication_candidate_absent"),
                            (replace(snapshot, freeze_commit="7" * 40), "publication_freeze_mismatch")):
            for strict, reader in ((True, "load_candidate"), (False, "load_candidate_at_branch_head")):
                with self.subTest(code=code, strict=strict), \
                        patch.object(preflight, reader, return_value=value) as loaded:
                    self.assertRefused(freeze, code, strict=strict)
                    self.assertEqual(loaded.call_count, 1)

    def test_canonical_selection_and_readiness_failures_keep_their_identity(self):
        freeze = self.produce()
        snapshot = self.loaded()
        with patch.object(preflight, "load_candidate", return_value=replace(snapshot, reviews=())):
            error = self.assertRefused(freeze, "selected_review_invalid")
        self.assertEqual(error.stage, check.STAGE)
        findings = (FindingRef(code="attempt_incomplete", subject="1"),)
        real = preflight.evaluate_readiness
        with patch.object(preflight, "evaluate_readiness",
                          side_effect=lambda **kw: replace(real(**kw), ready=False, findings=findings)):
            denied = self.assertRefused(freeze, "publication_readiness_denied")
        self.assertIsInstance(denied, preflight.PublicationNotReady)
        self.assertEqual(denied.findings, findings)
        fault = VerificationError("candidate_field_invalid", "candidate-readiness")
        with patch.object(preflight, "evaluate_readiness", side_effect=fault):
            with self.assertRaises(VerificationError) as caught:
                self.run_preflight(freeze)
        self.assertIs(caught.exception, fault)


if __name__ == "__main__":
    unittest.main()
