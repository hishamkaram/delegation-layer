"""Go-parity and closure-scan tests for checkpoint workspace operation records."""

from __future__ import annotations

from copy import deepcopy
import fcntl
from pathlib import Path
import shutil
import tempfile
import unittest
from typing import NotRequired, TypedDict
from scripts.acceptance_tests.record_fixtures import object_at

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.lifetime.closure import validate_managed_lifetime_closure
from scripts.delegate_acceptance.workspaces import checkpoint_records as records, specification
from scripts.delegate_acceptance.workspaces.closure import (
    validate_managed_workspace_operation_closure,
)
from scripts.delegate_acceptance.workspaces.specification import (
    _canonical_operation_request_bytes,
)


# Owned by internal/workspace/contract/checkpoint_operation_test.go: the
# historical request digests recorded before checkpoint kinds existed.
HISTORICAL_DIGESTS = {
    "provision": "dbcc08af17c014a0b6bad2720c3228358010f6d3510596f7293ad8fb4294a66f",
    "capture": "27c367f214d1f16117250ae4ec19e5204ad110829472dbb4deefbcde06cdcd21",
    "verify": "1f5b3616fa9e24d029f4815f3ed0960576de15aa83914105df65aad2ec58c3c7",
    "remove": "8e959a48d758e87fa37dc22a21b22aec3db296718cbf42fae98357194e782863",
    "forget": "e286566de374bed41635ddf97d6588a45e40445d932e3e58f5c9619447b279c9",
}

# Produced by go1.27.2 task.MarshalCanonical after the contract validators
# accepted the same values; see the parity fixtures below.
GO_DIGESTS = {
    "request-ordinary": "19b429fa1cbf4f1a9a3065a95b9f4476feb94f397334de705bc418488f32123b",
    "request-managed": "3c5aafc2b3e0f610f35e70fbc64030975efa2809103bc89f3de45751ab543ef9",
    "request-removal": "eca0079f53910ccac941304b00568696f92d3cc9146a109e9cb8c9e5e6bfa5fb",
    "candidate-ordinary": "3d94ca6727af8d3abb2a30869f55505f0264444c4cea38f581dca2bd9331d373",
    "candidate-managed": "40183850b52e430b2eb58b208a7c7cdda60f957fd23612e8d4fbcc7f9e5c6240",
    "receipt-checkpointed": "c7d84dbec306d0eaaf195e4168357afa2f782796c0ca090fa4e8bf3bb0ef145d",
    "receipt-unavailable": "5fd7f9ea5987d8fc7b5596f5e7647c66159ccc243d9ac7b95ae3e1989353f8e2",
    "removal-attempt": "87d1d278d7ca256b1e45bcc60e7bf9a5d1a66312554c2002878f515e6d26ddbc",
    "removal-receipt-removed": "aab25e8f1fa21fcb8a4c7c831c611dfd0c25b9854bcc5ee8533226b9c2ba600b",
    "removal-receipt-unavailable": "747a659532c43f7b4efd47820e0aeec4369907f358bb086f49106ab18c2495e8",
}

GO_ROOT = "1" * 32
GO_CHECKPOINT = "2" * 32
GO_REMOVAL = "a" * 32
DEADLINE = "2026-09-15T00:01:00Z"
BOOT = "linux:00000000-0000-4000-8000-000000000001"
OTHER_BOOT = "linux:00000000-0000-4000-8000-000000000002"


def go_request(kind: str) -> dict[str, object]:
    """Project contract operationFixture, the Go-owned historical request."""
    request: dict[str, object] = {
        "schema_version": 1, "root_id": GO_ROOT, "operation_id": GO_CHECKPOINT,
        "workspace_id": "3" * 32, "kind": kind,
        "subject": {"kind": "operation", "id": "4" * 32, "sha256": "5" * 64},
        "deadline": "2026-10-21T12:00:00Z", "worker_executable": "/fixture/delegate",
        "worker_sha256": "6" * 64,
        "supervisor": {
            "client_executable": "/fixture/pueue-client", "client_sha256": "7" * 64,
            "resolved_config_sha256": "8" * 64, "endpoint": "unix:/fixture/pueue.sock",
            "config_path": "/fixture/pueue.conf", "config_digest": "9" * 64,
            "observed_version": "fixture",
        },
    }
    if kind == "verify":
        request["arguments"] = ["make", "test"]
    if kind == "remove":
        request["discard"] = True
    return request


def ordinary_request(root: str = GO_ROOT, operation: str = GO_CHECKPOINT) -> dict[str, object]:
    request = go_request("checkpoint")
    request.update(root_id=root, operation_id=operation, workspace_id="", checkpoint={
        "kind": "ordinary", "cwd": "/repo/sub", "cwd_identity": {"device": 0, "inode": 3}})
    return request


def managed_request() -> dict[str, object]:
    request = go_request("checkpoint")
    object_at(request, "subject")["kind"] = "task"
    request["checkpoint"] = {"kind": "managed"}
    return request


def removal_request(checkpoint: str, checkpoint_digest: str, *, root: str = GO_ROOT,
                    operation: str = GO_REMOVAL) -> dict[str, object]:
    request = go_request("checkpoint-remove")
    request.update(root_id=root, operation_id=operation, workspace_id="", subject={
        "kind": "operation", "id": checkpoint, "sha256": checkpoint_digest})
    return request


def request_bytes(request: dict[str, object]) -> bytes:
    return _canonical_operation_request_bytes(
        request, str(request["root_id"]), str(request["operation_id"]))


def commit_payload(operation: str, request_digest: str, *, message: str = "Delegate checkpoint <&>",
                   tree: str = "c" * 40, parent: str = "b" * 40) -> str:
    return (f"tree {tree}\nparent {parent}\n"
            "author Delegate <delegate@localhost> 1761048000 +0000\n"
            "committer Delegate <delegate@localhost> 1761048000 +0000\n\n"
            f"{message}\n\nDelegate-Checkpoint: {operation}\n"
            f"Delegate-Checkpoint-Request: {request_digest}\n")


def ordinary_target() -> dict[str, object]:
    return {"source": {
        "Root": "/repo", "CommonDir": "/repo/.git", "GitDir": "/repo/.git", "RelativeCwd": "sub",
        "BaseOID": "a" * 40, "ObjectFormat": "sha1", "Dirty": True, "CommonDirInode": 2,
    }, "checkout_root": "/repo", "admin_dir": "/repo/.git"}


def managed_target() -> dict[str, object]:
    return {"source": {
        "Root": "/source", "CommonDir": "/source/.git", "GitDir": "/source/.git",
        "RelativeCwd": ".", "BaseOID": "A" * 40, "ObjectFormat": "sha1", "Dirty": False,
        "CommonDirDevice": 7, "CommonDirInode": 2,
    }, "checkout_root": "/container/workspace", "admin_dir": "/source/.git/worktrees/workspace"}


def candidate(request: dict[str, object], request_digest: str,
              target: dict[str, object]) -> dict[str, object]:
    operation = str(request["operation_id"])
    return {
        "schema_version": 1, "operation_id": operation, "request_sha256": request_digest,
        "target": target, "observed_head": "b" * 40, "index_sha256": "e" * 64,
        "tree_oid": "c" * 40, "commit_oid": "d" * 40,
        "snapshot_ref": f"refs/delegate/{request['root_id']}/checkpoints/{operation}",
        "commit_payload": commit_payload(operation, request_digest),
    }


def checkpointed_receipt(value: dict[str, object], termination: str, wait: str) -> dict[str, object]:
    return {"schema_version": 1, "operation_id": value["operation_id"],
            "request_sha256": value["request_sha256"], "status": "checkpointed",
            "candidate": value, "termination_sha256": termination, "worker_wait_sha256": wait}


def attempt(checkpoint: str, sequence: int, removal: str, removal_digest: str) -> dict[str, object]:
    return {"schema_version": 1, "checkpoint_id": checkpoint, "sequence": sequence,
            "operation_id": removal, "request_sha256": removal_digest}


class ReceiptJournalOptions(TypedDict):
    cause: NotRequired[str]
    wait: NotRequired[str]
    termination: NotRequired[str | None]
    legacy: NotRequired[bool]
    success: NotRequired[bool]


class CheckpointCodecParityTests(unittest.TestCase):
    def test_installed_operation_identity_omits_digest_in_go_order(self) -> None:
        legacy = request_bytes(go_request("capture"))
        request = go_request("capture")
        del request["worker_sha256"]
        request["executable_identity"] = "installed-path-v1"
        expected = legacy.replace(
            b'"worker_sha256":"' + b"6" * 64 + b'"',
            b'"executable_identity":"installed-path-v1"')
        self.assertEqual(request_bytes(request), expected)
        request["worker_sha256"] = ""
        self.assertEqual(request_bytes(request), expected)

    def test_operation_identity_refuses_unknown_mixed_null_and_truncated_legacy(self) -> None:
        for changes in (
                {"executable_identity": "unknown"},
                {"executable_identity": None},
                {"executable_identity": "installed-path-v1"},
                {"executable_identity": "installed-path-v1", "worker_sha256": None},
                {"worker_sha256": ""},
        ):
            with self.subTest(changes=changes):
                request = go_request("capture")
                request.update(changes)
                with self.assertRaises(AcceptanceFailure):
                    request_bytes(request)
        request = go_request("capture")
        del request["worker_sha256"]
        with self.assertRaises(AcceptanceFailure):
            request_bytes(request)

    def test_malformed_subject_and_operation_kinds_are_protocol_failures(self) -> None:
        for invalid in ([], {}, None):
            subject = {"kind": invalid, "id": GO_CHECKPOINT, "sha256": "a" * 64}
            for validate in (specification._canonical_workspace_subject,
                             specification._canonical_operation_subject):
                with self.subTest(validator=validate.__name__, invalid=invalid):
                    with self.assertRaisesRegex(AcceptanceFailure, "subject kind is invalid"):
                        validate(subject, "subject")
            request = go_request("capture")
            request["kind"] = invalid
            with self.subTest(operation_kind=invalid):
                with self.assertRaisesRegex(AcceptanceFailure, "kind is invalid"):
                    request_bytes(request)

    def test_malformed_checkpoint_format_and_status_are_protocol_failures(self) -> None:
        request = ordinary_request()
        digest = sha(request_bytes(request))
        for invalid in ([], {}, None):
            target = ordinary_target()
            source = target["source"]
            assert isinstance(source, dict)
            source["ObjectFormat"] = invalid
            with self.subTest(object_format=invalid):
                with self.assertRaisesRegex(AcceptanceFailure, "object format is invalid"):
                    records.canonical_checkpoint_candidate_bytes(
                        candidate(request, digest, target), request, digest, "candidate")
            receipt = checkpointed_receipt(
                candidate(request, digest, ordinary_target()), "f" * 64, "0" * 64)
            receipt["status"] = invalid
            with self.subTest(status=invalid):
                with self.assertRaisesRegex(AcceptanceFailure, "status is invalid"):
                    records.canonical_checkpoint_receipt_bytes(receipt, request, digest, "receipt")

    def test_malformed_closure_and_result_states_are_protocol_failures(self) -> None:
        for invalid in ([], {}, None):
            closure = {"schema_version": 1, "workspace_id": GO_CHECKPOINT,
                       "sequence": 1, "disposition": invalid}
            with self.subTest(disposition=invalid):
                with self.assertRaisesRegex(AcceptanceFailure, "disposition is invalid"):
                    specification._canonical_workspace_closure_bytes(
                        closure, GO_CHECKPOINT, 1, "closure")
            result = {"schema_version": 1, "operation_id": GO_CHECKPOINT,
                      "request_sha256": "a" * 64, "status": invalid}
            with self.subTest(result_status=invalid):
                with self.assertRaisesRegex(AcceptanceFailure, "binding is invalid"):
                    specification._canonical_operation_result_bytes(
                        result, GO_CHECKPOINT, "a" * 64, "capture", "result")

    def test_historical_kinds_keep_go_owned_digests_and_workspace_scope(self) -> None:
        for kind, digest in HISTORICAL_DIGESTS.items():
            with self.subTest(kind=kind):
                self.assertEqual(sha(request_bytes(go_request(kind))), digest)
                for selection in ({"kind": "managed"}, ordinary_request()["checkpoint"], None):
                    invalid = go_request(kind)
                    invalid["checkpoint"] = selection
                    with self.assertRaises(AcceptanceFailure):
                        request_bytes(invalid)
                invalid = go_request(kind)
                invalid["workspace_id"] = ""
                with self.assertRaises(AcceptanceFailure):
                    request_bytes(invalid)

    def test_checkpoint_requests_match_go_bytes(self) -> None:
        ordinary = request_bytes(ordinary_request())
        self.assertEqual(sha(ordinary), GO_DIGESTS["request-ordinary"])
        self.assertIn(b'"workspace_id":""', ordinary)
        managed = request_bytes(managed_request())
        self.assertEqual(sha(managed), GO_DIGESTS["request-managed"])
        self.assertTrue(managed.endswith(b'"checkpoint":{"kind":"managed"}}\n'))
        removal = request_bytes(removal_request(GO_CHECKPOINT, sha(ordinary)))
        self.assertEqual(sha(removal), GO_DIGESTS["request-removal"])
        self.assertNotIn(b'"checkpoint"', removal)

    def test_checkpoint_request_scope_refusals(self) -> None:
        def ordinary(**changes: object) -> dict[str, object]:
            request = ordinary_request()
            object_at(request, "checkpoint").update(changes)
            return request

        managed_with_workspace = ordinary_request()
        managed_with_workspace["workspace_id"] = "3" * 32
        foreign_managed = managed_request()
        foreign_managed["workspace_id"] = "not-a-workspace"
        empty_managed = managed_request()
        empty_managed["workspace_id"] = ""
        missing = managed_request()
        del missing["checkpoint"]
        null_selection = managed_request()
        null_selection["checkpoint"] = None
        removal_workspace = removal_request(GO_CHECKPOINT, "f" * 64)
        removal_workspace["workspace_id"] = "3" * 32
        removal_task = removal_request(GO_CHECKPOINT, "f" * 64)
        object_at(removal_task, "subject")["kind"] = "task"
        removal_selection = removal_request(GO_CHECKPOINT, "f" * 64)
        removal_selection["checkpoint"] = {"kind": "managed"}
        cases = {
            "ordinary with workspace": managed_with_workspace,
            "managed foreign workspace": foreign_managed,
            "managed empty workspace": empty_managed,
            "missing selection": missing,
            "null selection": null_selection,
            "unknown selector": ordinary(kind="detached"),
            "unknown selection field": ordinary(extra=True),
            "unclean cwd": ordinary(cwd="/repo/../sub"),
            "relative cwd": ordinary(cwd="repo/sub"),
            "empty cwd": ordinary(cwd=""),
            "null identity": ordinary(cwd_identity=None),
            "zero inode": ordinary(cwd_identity={"device": 1, "inode": 0}),
            "boolean device": ordinary(cwd_identity={"device": True, "inode": 3}),
            "overflow inode": ordinary(cwd_identity={"device": 1, "inode": 1 << 64}),
            "identity extra": ordinary(cwd_identity={"device": 1, "inode": 3, "x": 1}),
            "managed with cwd": {**managed_request(), "checkpoint": {"kind": "managed", "cwd": "/repo"}},
            "managed empty cwd": {**managed_request(), "checkpoint": {"kind": "managed", "cwd": ""}},
            "removal workspace": removal_workspace,
            "removal task subject": removal_task,
            "removal selection": removal_selection,
        }
        for name, request in cases.items():
            with self.subTest(name), self.assertRaises(AcceptanceFailure):
                request_bytes(request)

    def test_candidates_and_receipts_match_go_bytes(self) -> None:
        ordinary = ordinary_request()
        ordinary_digest = sha(request_bytes(ordinary))
        managed = managed_request()
        managed_digest = sha(request_bytes(managed))
        ordinary_candidate = candidate(ordinary, ordinary_digest, ordinary_target())
        encoded = records.canonical_checkpoint_candidate_bytes(
            ordinary_candidate, ordinary, ordinary_digest, "candidate")
        self.assertEqual(sha(encoded), GO_DIGESTS["candidate-ordinary"])
        self.assertIn(b"\\u003c\\u0026\\u003e", encoded)
        self.assertEqual(sha(records.canonical_checkpoint_candidate_bytes(
            candidate(managed, managed_digest, managed_target()), managed, managed_digest,
            "candidate")), GO_DIGESTS["candidate-managed"])
        receipt = checkpointed_receipt(ordinary_candidate, "f" * 64, "0" * 64)
        self.assertEqual(sha(records.canonical_checkpoint_receipt_bytes(
            receipt, ordinary, ordinary_digest, "receipt")), GO_DIGESTS["receipt-checkpointed"])
        unavailable = {
            "lifetime_closure": "unregistered-recovered", "schema_version": 1,
            "operation_id": GO_CHECKPOINT, "request_sha256": ordinary_digest,
            "status": "unavailable", "reason_code": "checkpoint_not_launched",
            "worker_wait_sha256": "0" * 64,
        }
        self.assertEqual(sha(records.canonical_checkpoint_receipt_bytes(
            unavailable, ordinary, ordinary_digest, "receipt")), GO_DIGESTS["receipt-unavailable"])

    def test_removal_records_match_go_bytes(self) -> None:
        ordinary_digest = sha(request_bytes(ordinary_request()))
        removal_digest = sha(request_bytes(removal_request(GO_CHECKPOINT, ordinary_digest)))
        reference = attempt(GO_CHECKPOINT, 1, GO_REMOVAL, removal_digest)
        self.assertEqual(sha(records.canonical_checkpoint_removal_reference_bytes(
            reference, GO_CHECKPOINT, 1, "attempt")), GO_DIGESTS["removal-attempt"])
        removed = {"schema_version": 1, "attempt": reference, "status": "removed",
                   "termination_sha256": "f" * 64, "worker_wait_sha256": "0" * 64}
        self.assertEqual(sha(records.canonical_checkpoint_removal_receipt_bytes(
            removed, reference, "receipt")), GO_DIGESTS["removal-receipt-removed"])
        failed = {"lifetime_closure": "legacy-previous-boot", "schema_version": 1,
                  "attempt": reference, "status": "unavailable",
                  "reason_code": "checkpoint_removal_result_unavailable",
                  "termination_sha256": "f" * 64}
        self.assertEqual(sha(records.canonical_checkpoint_removal_receipt_bytes(
            failed, reference, "receipt")), GO_DIGESTS["removal-receipt-unavailable"])
        for name, change in {
                "foreign attempt": {"attempt": {**reference, "operation_id": "b" * 32}},
                "other sequence": {"attempt": {**reference, "sequence": 2}},
                "success with reason": {"reason_code": "checkpoint_removal_failed"},
                "recovered success": {"lifetime_closure": "unregistered-recovered",
                                      "termination_sha256": None},
                "success without wait": {"worker_wait_sha256": None},
                "unknown status": {"status": "deleted"},
        }.items():
            invalid = {**removed, **change}
            invalid = {key: value for key, value in invalid.items() if value is not None}
            with self.subTest(name), self.assertRaises(AcceptanceFailure):
                records.canonical_checkpoint_removal_receipt_bytes(invalid, reference, "receipt")

    def test_candidate_refuses_foreign_or_corrupt_shapes(self) -> None:
        request = ordinary_request()
        digest = sha(request_bytes(request))
        valid = candidate(request, digest, ordinary_target())

        def changed(path: tuple[str, ...], value: object) -> dict[str, object]:
            result = deepcopy(valid)
            owner = object_at(result, *path[:-1])
            if value is KeyError:
                del owner[path[-1]]
            else:
                owner[path[-1]] = value
            return result

        cases = {
            "foreign request digest": changed(("request_sha256",), "f" * 64),
            "foreign operation": changed(("operation_id",), "9" * 32),
            "foreign ref": changed(("snapshot_ref",), f"refs/delegate/{GO_ROOT}/checkpoints/{'9' * 32}"),
            "workspace ref": changed(("snapshot_ref",), f"refs/delegate/{GO_ROOT}/workspaces/{GO_CHECKPOINT}"),
            "short oid": changed(("tree_oid",), "c" * 39),
            "sha256 width": changed(("commit_oid",), "d" * 64),
            "index digest": changed(("index_sha256",), "E" * 64),
            "unknown field": changed(("extra",), 1),
            "null target": changed(("target",), None),
            "unpinned common dir": changed(("target", "source", "CommonDirInode"), KeyError),
            "zero device present": changed(("target", "source", "CommonDirDevice"), 0),
            "missing Dirty": changed(("target", "source", "Dirty"), KeyError),
            "escaping relative cwd": changed(("target", "source", "RelativeCwd"), "../sub"),
            "ordinary cwd mismatch": changed(("target", "source", "RelativeCwd"), "other"),
            "ordinary admin mismatch": changed(("target", "admin_dir"), "/repo/.git/worktrees/x"),
            "payload tree": changed(("commit_payload",), commit_payload(
                GO_CHECKPOINT, digest, tree="9" * 40)),
            "payload request": changed(("commit_payload",), commit_payload(GO_CHECKPOINT, "f" * 64)),
            "payload extra trailer": changed(("commit_payload",), commit_payload(
                GO_CHECKPOINT, digest, message="Delegate-Checkpoint-Note: x")),
            "payload non-ascii": changed(("commit_payload",), commit_payload(
                GO_CHECKPOINT, digest, message="café")),
            "payload bound": changed(("commit_payload",), commit_payload(
                GO_CHECKPOINT, digest, message="x" * 4096)),
        }
        for name, value in cases.items():
            with self.subTest(name), self.assertRaises(AcceptanceFailure):
                records.canonical_checkpoint_candidate_bytes(value, request, digest, "candidate")
        managed = managed_request()
        managed_digest = sha(request_bytes(managed))
        disguised = candidate(managed, managed_digest, managed_target())
        object_at(disguised, "target")["checkout_root"] = "/source"
        with self.assertRaises(AcceptanceFailure):
            records.canonical_checkpoint_candidate_bytes(disguised, managed, managed_digest, "candidate")
        with self.assertRaises(AcceptanceFailure):
            records.canonical_checkpoint_candidate_bytes(
                valid, go_request("capture"), digest, "candidate")

    def test_receipt_refuses_success_without_native_encoding(self) -> None:
        request = ordinary_request()
        digest = sha(request_bytes(request))
        valid = checkpointed_receipt(candidate(request, digest, ordinary_target()), "f" * 64, "0" * 64)
        cases = {
            "recovered kind": {**valid, "lifetime_closure": "unregistered-recovered"},
            "registered kind": {**valid, "lifetime_closure": "registered-native"},
            "empty kind": {**valid, "lifetime_closure": ""},
            "no wait": {key: value for key, value in valid.items() if key != "worker_wait_sha256"},
            "unlaunched": {**valid, "unlaunched_sha256": "1" * 64},
            "reason": {**valid, "reason_code": "checkpoint_failed"},
            "null candidate": {**valid, "candidate": None},
            "missing candidate": {key: value for key, value in valid.items() if key != "candidate"},
            "null reason": {**valid, "reason_code": None},
            "unavailable with candidate": {**valid, "status": "unavailable",
                                           "reason_code": "checkpoint_failed"},
            "not launched after run": {**{key: value for key, value in valid.items()
                                          if key != "candidate"},
                                       "status": "unavailable",
                                       "reason_code": "checkpoint_not_launched"},
        }
        for name, value in cases.items():
            with self.subTest(name), self.assertRaises(AcceptanceFailure):
                records.canonical_checkpoint_receipt_bytes(value, request, digest, "receipt")

    def test_removal_names_parse_exactly(self) -> None:
        self.assertEqual(records.checkpoint_removal_record("removal-attempt-00000000000000000001.json"),
                         ("attempt", 1))
        self.assertEqual(records.checkpoint_removal_record("removal-receipt-18446744073709551615.json"),
                         ("receipt", (1 << 64) - 1))
        self.assertIsNone(records.checkpoint_removal_record("request.json"))
        for name in ("removal-attempt-1.json", "removal-attempt-00000000000000000000.json",
                     "removal-attempt-18446744073709551616.json",
                     "removal-attempt-0000000000000000000x.json",
                     "removal-attempt-00000000000000000001.jso",
                     "removal-attempt-0000000000000000000¹.json",
                     "removal-other-00000000000000000001.json", "removal-"):
            with self.subTest(name), self.assertRaises(AcceptanceFailure):
                records.checkpoint_removal_record(name)


ROOT = "1" * 32
CHECKPOINT = "2" * 32
REMOVAL = "a" * 32
OTHER_REMOVAL = "b" * 32


def write_record(path: Path, value: object) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    path.write_bytes(canonical_go_json(value))
    path.chmod(0o600)


def lifetime_records(operation: str, request_digest: str) -> dict[str, dict[str, object]]:
    """Return one closed workspace-operation lifetime, as the shared validator expects."""
    owner = {"schema_version": 1, "root_id": ROOT, "task_id": operation, "owner_kind": "workspace",
             "owner_sha256": request_digest, "scope_kind": "process-group",
             "assurance": "cooperative", "boot_id": BOOT, "deadline": DEADLINE}
    intent = {**owner, "scope_plan": "fixture-plan", "created_at": "2026-09-15T00:00:01Z"}
    registration = {**owner, "scope_id": "fixture-scope", "created_at": "2026-09-15T00:00:02Z"}
    registration_digest = sha(canonical_go_json(registration))
    worker = {"schema_version": 1, "root_id": ROOT, "task_id": operation,
              "registration_sha256": registration_digest, "pid": 2001,
              "process_start": "fixture-worker", "boot_id": BOOT,
              "started_at": "2026-09-15T00:00:03Z"}
    launch = {"schema_version": 1, "root_id": ROOT, "task_id": operation,
              "registration_sha256": registration_digest, "sequence": 1, "pid": 2002,
              "process_start": "fixture-launch", "pgid": 2002, "started_at": "2026-09-15T00:00:04Z"}
    termination = {"schema_version": 1, "root_id": ROOT, "task_id": operation,
                   "registration_sha256": registration_digest, "launch_count": 1,
                   "launch_set_sha256": sha(canonical_go_json([launch])), "cause": "completed",
                   "scope_empty": True, "observed_at": "2026-09-15T00:00:06Z"}
    wait = {"schema_version": 1, "root_id": ROOT, "task_id": operation,
            "registration_sha256": registration_digest,
            "worker_sha256": sha(canonical_go_json(worker)), "child_pid": 2001,
            "result": "success", "exit_code": 0, "observed_at": "2026-09-15T00:00:05Z"}
    return {
        "lifetime-intent.json": intent, "lifetime-registration.json": registration,
        "lifetime-worker.json": worker, "lifetime-launch-00000000000000000001.json": launch,
        "lifetime-termination.json": termination, "lifetime-worker-wait.json": wait,
        "start.json": {"schema_version": 1, "request_sha256": request_digest},
    }


def recovered_wait(wait: dict[str, object], *, proof: str = "exact-worker",
                   observed_boot: str = BOOT) -> dict[str, object]:
    """Replace a native Wait with a registered worker absence."""
    recovered = {key: value for key, value in wait.items() if key != "exit_code"}
    recovered.update(result="absent", observed_at="2026-09-14T00:00:00Z",
                     absence_proof=proof, observed_boot_id=observed_boot)
    return recovered


def unregistered_lifetime(lifetime: dict[str, dict[str, object]]) -> dict[str, dict[str, object]]:
    """Reduce a lifetime to its intent and an unregistered-disposed absence."""
    intent = lifetime["lifetime-intent.json"]
    return {"lifetime-intent.json": intent, "lifetime-worker-wait.json": {
        "schema_version": 1, "root_id": ROOT, "task_id": intent["task_id"],
        "registration_sha256": "", "child_pid": 0, "result": "absent",
        "observed_at": "2026-09-14T00:00:00Z", "absence_proof": "unregistered-disposed",
        "observed_boot_id": BOOT, "intent_sha256": sha(canonical_go_json(intent))}}


class CheckpointClosureScanTests(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.state = Path(temporary.name) / "state"
        self.state.mkdir(mode=0o700)
        self.operations = self.state / "workspace-operations"
        write_record(self.state / "root.json", {
            "schema_version": 1, "root_id": ROOT, "created_at": "2026-09-15T00:00:00Z"})

    def journal(self, request: dict[str, object]) -> tuple[Path, str]:
        request = {**request, "deadline": DEADLINE}
        directory = self.operations / str(request["operation_id"])
        directory.mkdir(mode=0o700, parents=True)
        data = request_bytes(request)
        (directory / "request.json").write_bytes(data)
        (directory / "request.json").chmod(0o600)
        return directory, sha(data)

    def close(self, directory: Path, request_digest: str, *, wait: str = "native",
              cause: str = "completed") -> tuple[str, str]:
        """Write a closed lifetime and return its termination and Wait record digests.

        wait is native, recovered (exact worker), previous-boot (absence on
        another boot), missing, or unregistered (intent-only absence).
        """
        lifetime = lifetime_records(directory.name, request_digest)
        object_at(lifetime, "lifetime-termination.json")["cause"] = cause
        if wait == "recovered":
            lifetime["lifetime-worker-wait.json"] = recovered_wait(lifetime["lifetime-worker-wait.json"])
        if wait == "previous-boot":
            lifetime["lifetime-worker-wait.json"] = recovered_wait(
                lifetime["lifetime-worker-wait.json"], proof="previous-boot", observed_boot=OTHER_BOOT)
        if wait == "unregistered":
            lifetime = unregistered_lifetime(lifetime)
        for name, value in lifetime.items():
            if wait != "missing" or name != "lifetime-worker-wait.json":
                write_record(directory / name, value)
        termination = lifetime.get("lifetime-termination.json")
        return ("" if termination is None else sha(canonical_go_json(termination)),
                sha(canonical_go_json(lifetime["lifetime-worker-wait.json"])))

    def publish_previous_boot_absence(self, directory: Path, *, observed_boot: str = OTHER_BOOT) -> None:
        """Publish the later worker absence over a closed lifetime that has no Wait."""
        lifetime = lifetime_records(directory.name, sha((directory / "request.json").read_bytes()))
        write_record(directory / "lifetime-worker-wait.json", recovered_wait(
            lifetime["lifetime-worker-wait.json"], proof="previous-boot", observed_boot=observed_boot))

    def receipt_journal(self, path: str, *, cause: str = "previous-boot", wait: str = "missing",
                        legacy: bool = True, success: bool = False,
                        termination: str | None = None) -> tuple[Path, Path]:
        """Write a creation or sole removal journal with one canonical stored receipt.

        Return the journal whose lifetime the receipt closes and the receipt path.
        A legacy receipt names only the termination, as the coordinator records a
        registered previous-boot termination without a worker Wait.
        """
        if path == "creation":
            directory, digest = self.journal(ordinary_request(ROOT, CHECKPOINT))
        else:
            _, creation_digest, _ = self.checkpoint()
            directory, digest = self.journal(removal_request(CHECKPOINT, creation_digest, root=ROOT))
        actual_termination, wait_digest = self.close(directory, digest, wait=wait, cause=cause)
        closure: dict[str, object] = {"termination_sha256": termination or actual_termination}
        if legacy:
            closure["lifetime_closure"] = "legacy-previous-boot"
        else:
            closure["worker_wait_sha256"] = wait_digest
        if path == "creation":
            request = {**ordinary_request(ROOT, CHECKPOINT), "deadline": DEADLINE}
            value = candidate(request, digest, ordinary_target())
            write_record(directory / records.CHECKPOINT_CANDIDATE_RECORD, value)
            receipt = {"schema_version": 1, "operation_id": CHECKPOINT, "request_sha256": digest,
                       **closure, **({"status": "checkpointed", "candidate": value} if success else {
                           "status": "unavailable", "reason_code": "checkpoint_result_unavailable"})}
            receipt_path = directory / records.CHECKPOINT_RECEIPT_RECORD
            data = records.canonical_checkpoint_receipt_bytes(receipt, request, digest, "fixture receipt")
        else:
            reference = attempt(CHECKPOINT, 1, REMOVAL, digest)
            creation = self.operations / CHECKPOINT
            write_record(creation / records.checkpoint_removal_name("attempt", 1), reference)
            receipt = {"schema_version": 1, "attempt": reference, **closure,
                       **({"status": "removed"} if success else {
                           "status": "unavailable", "reason_code": "checkpoint_removal_failed"})}
            receipt_path = creation / records.checkpoint_removal_name("receipt", 1)
            data = records.canonical_checkpoint_removal_receipt_bytes(
                receipt, reference, "fixture removal receipt")
        # The canonical encoders restore Go field order for the merged fields.
        receipt_path.write_bytes(data)
        receipt_path.chmod(0o600)
        return directory, receipt_path

    def receipt_status(self, path: str) -> object:
        checkpoint = object_at(self.scan(), "workspace", CHECKPOINT, "checkpoint")
        return checkpoint["status"] if path == "creation" else object_at(checkpoint, "removals", 0)["status"]

    def checkpoint(self, *, wait: str = "native", receipt: bool = True) -> tuple[Path, str, dict[str, object]]:
        directory, digest = self.journal(ordinary_request(ROOT, CHECKPOINT))
        termination, wait_digest = self.close(directory, digest, wait=wait)
        value = candidate({**ordinary_request(ROOT, CHECKPOINT)}, digest, ordinary_target())
        write_record(directory / records.CHECKPOINT_CANDIDATE_RECORD, value)
        if receipt:
            write_record(directory / records.CHECKPOINT_RECEIPT_RECORD,
                         checkpointed_receipt(value, termination, wait_digest))
        return directory, digest, value

    def removal(self, checkpoint_digest: str, *, operation: str = REMOVAL, sequence: int | None = 1,
                status: str | None = "removed", wait: str = "native") -> tuple[Path, str]:
        """Write one removal journal and, unless unregistered, its attempt and receipt.

        wait="unsubmitted" leaves the removal journal with only its request.
        """
        directory, digest = self.journal(removal_request(
            CHECKPOINT, checkpoint_digest, root=ROOT, operation=operation))
        termination = wait_digest = ""
        if wait != "unsubmitted":
            termination, wait_digest = self.close(directory, digest, wait=wait)
        if sequence is None:
            return directory, digest
        reference = attempt(CHECKPOINT, sequence, operation, digest)
        creation = self.operations / CHECKPOINT
        write_record(creation / records.checkpoint_removal_name("attempt", sequence), reference)
        if status is not None:
            receipt = {"schema_version": 1, "attempt": reference, "status": status,
                       "termination_sha256": termination, "worker_wait_sha256": wait_digest}
            if status == "unavailable":
                receipt["reason_code"] = "checkpoint_removal_failed"
            # The canonical encoder restores Go field order after the late reason_code.
            path = creation / records.checkpoint_removal_name("receipt", sequence)
            path.write_bytes(records.canonical_checkpoint_removal_receipt_bytes(
                receipt, reference, "fixture removal receipt"))
            path.chmod(0o600)
        return directory, digest

    def scan(self) -> dict[str, object]:
        return validate_managed_lifetime_closure(self.state)

    def test_checkpoint_only_root_is_scanned_with_native_success(self) -> None:
        _, digest, _ = self.checkpoint()
        self.removal(digest)
        closure = self.scan()
        self.assertEqual(closure["tasks"], {})
        creation = object_at(closure, "workspace", CHECKPOINT)
        self.assertEqual(creation["mode"], "terminated")
        self.assertEqual(creation["checkpoint"], {
            "candidate": True, "status": "checkpointed",
            "removals": [{"sequence": 1, "operation_id": REMOVAL, "status": "removed"}]})
        self.assertEqual(object_at(closure, "workspace", REMOVAL)["checkpoint_removal"],
                         {"checkpoint_id": CHECKPOINT, "sequence": 1})

    def test_pending_or_missing_wait_never_qualifies(self) -> None:
        directory, _, _ = self.checkpoint(wait="missing")
        with self.assertRaises(AcceptanceFailure):
            self.scan()
        (directory / "lifetime-worker-wait.json").unlink(missing_ok=True)
        lock = directory / ".lifetime-owner.lock"
        lock.write_bytes(b"")
        lifetime = lifetime_records(CHECKPOINT, sha((directory / "request.json").read_bytes()))
        write_record(directory / "lifetime-worker-wait.json", lifetime["lifetime-worker-wait.json"])
        with lock.open("r+b") as held:
            fcntl.flock(held.fileno(), fcntl.LOCK_EX)
            with self.assertRaises(AcceptanceFailure):
                self.scan()
            fcntl.flock(held.fileno(), fcntl.LOCK_UN)
        self.assertEqual(object_at(self.scan(), "workspace", CHECKPOINT, "checkpoint")["status"], "checkpointed")

    def test_recovered_absence_cannot_be_advertised_as_success(self) -> None:
        directory, _, _ = self.checkpoint(wait="recovered")
        receipt = directory / records.CHECKPOINT_RECEIPT_RECORD
        stored = canonical_go_json(self.read(receipt))
        self.assertIn(b'"worker_wait_sha256"', stored)
        with self.assertRaises(AcceptanceFailure) as refused:
            self.scan()
        self.assertIn("native worker Wait", str(refused.exception))
        value = self.read(receipt)
        write_record(receipt, {
            "schema_version": 1, "operation_id": CHECKPOINT,
            "request_sha256": value["request_sha256"], "status": "unavailable",
            "reason_code": "checkpoint_result_unavailable",
            "termination_sha256": value["termination_sha256"],
            "worker_wait_sha256": value["worker_wait_sha256"]})
        self.assertEqual(object_at(self.scan(), "workspace", CHECKPOINT, "checkpoint")["status"], "unavailable")

    def test_receipt_closure_must_equal_actual_lifetime_records(self) -> None:
        directory, _, _ = self.checkpoint()
        receipt = directory / records.CHECKPOINT_RECEIPT_RECORD
        for field in ("termination_sha256", "worker_wait_sha256"):
            value = self.read(receipt)
            original = value[field]
            value[field] = "9" * 64
            write_record(receipt, value)
            with self.subTest(field), self.assertRaises(AcceptanceFailure):
                self.scan()
            value[field] = original
            write_record(receipt, value)
        self.scan()

    def test_unlaunched_receipt_binds_the_fence(self) -> None:
        directory, digest = self.journal(ordinary_request(ROOT, CHECKPOINT))
        fence = {"schema_version": 1, "root_id": ROOT, "task_id": CHECKPOINT,
                 "owner_kind": "workspace", "owner_sha256": digest, "deadline": DEADLINE,
                 "cause": "deadline-expired", "observed_at": "2026-09-15T00:01:01Z"}
        write_record(directory / "lifetime-unlaunched.json", fence)
        receipt = {"schema_version": 1, "operation_id": CHECKPOINT, "request_sha256": digest,
                   "status": "unavailable", "reason_code": "checkpoint_not_launched",
                   "unlaunched_sha256": sha(canonical_go_json(fence))}
        write_record(directory / records.CHECKPOINT_RECEIPT_RECORD, receipt)
        self.assertEqual(object_at(self.scan(), "workspace", CHECKPOINT, "checkpoint")["status"], "unavailable")
        receipt["unlaunched_sha256"] = "9" * 64
        write_record(directory / records.CHECKPOINT_RECEIPT_RECORD, receipt)
        with self.assertRaises(AcceptanceFailure):
            self.scan()

    def test_receipt_without_closure_and_corrupt_candidate_fail_closed(self) -> None:
        directory, digest = self.journal(ordinary_request(ROOT, CHECKPOINT))
        write_record(directory / records.CHECKPOINT_RECEIPT_RECORD, {
            "lifetime_closure": "unregistered-recovered", "schema_version": 1,
            "operation_id": CHECKPOINT, "request_sha256": digest, "status": "unavailable",
            "reason_code": "checkpoint_not_launched", "worker_wait_sha256": "0" * 64})
        with self.assertRaises(AcceptanceFailure):
            self.scan()
        (directory / records.CHECKPOINT_RECEIPT_RECORD).unlink()
        self.assertEqual(object_at(self.scan(), "workspace", CHECKPOINT)["mode"], "unsubmitted")
        for name, mutate in {
                "candidate bytes": lambda path: path.write_bytes(b"{ " + path.read_bytes()[1:]),
                "candidate missing": lambda path: path.unlink(),
                "candidate differs": lambda path: write_record(path, {
                    **self.read(path), "index_sha256": "9" * 64}),
        }.items():
            with self.subTest(name):
                self.reset()
                creation, _, _ = self.checkpoint()
                mutate(creation / records.CHECKPOINT_CANDIDATE_RECORD)
                with self.assertRaises(AcceptanceFailure):
                    self.scan()

    def test_removal_history_must_be_contiguous_and_bound(self) -> None:
        cases = {
            "noncontiguous": lambda digest: self.removal(digest, sequence=2, status=None),
            "orphan receipt": lambda digest: self.orphan_receipt(digest),
            "duplicate operation": lambda digest: self.duplicate_attempt(digest),
            "malformed name": lambda digest: write_record(
                self.operations / CHECKPOINT / "removal-attempt-1.json", {}),
            "removed without candidate": lambda digest: (
                self.removal(digest),
                (self.operations / CHECKPOINT / records.CHECKPOINT_CANDIDATE_RECORD).unlink(),
                (self.operations / CHECKPOINT / records.CHECKPOINT_RECEIPT_RECORD).unlink()),
            "recovered removal success": lambda digest: self.removal(digest, wait="recovered"),
            "foreign removal request": lambda digest: self.removal("9" * 64),
            "submitted unregistered removal": lambda digest: self.removal(
                digest, operation=OTHER_REMOVAL, sequence=None),
        }
        for name, build in cases.items():
            with self.subTest(name):
                self.reset()
                _, digest, _ = self.checkpoint()
                build(digest)
                with self.assertRaises(AcceptanceFailure):
                    self.scan()

    def test_pending_attempt_and_unsubmitted_loser_remain_cleanable(self) -> None:
        _, digest, _ = self.checkpoint()
        self.removal(digest, status=None)
        self.journal(removal_request(CHECKPOINT, digest, root=ROOT, operation=OTHER_REMOVAL))
        closure = self.scan()["workspace"]
        self.assertEqual(object_at(closure, CHECKPOINT, "checkpoint")["removals"],
                         [{"sequence": 1, "operation_id": REMOVAL, "status": None}])
        self.assertEqual(object_at(closure, OTHER_REMOVAL)["mode"], "unsubmitted")
        self.assertEqual(object_at(closure, OTHER_REMOVAL)["checkpoint_removal"],
                         {"checkpoint_id": CHECKPOINT, "sequence": None})

    def test_prior_removal_attempts_require_unavailable_receipts(self) -> None:
        for prior in (None, "removed", "nothing_to_remove"):
            with self.subTest(prior=prior):
                self.reset()
                _, digest, _ = self.checkpoint()
                self.removal(digest, status=prior)
                self.removal(digest, operation=OTHER_REMOVAL, sequence=2, status="nothing_to_remove")
                with self.assertRaisesRegex(
                        AcceptanceFailure, "prior attempt without an unavailable receipt"):
                    self.scan()

    def test_unavailable_prefix_keeps_head_outcomes(self) -> None:
        for head, wait in (("removed", "native"), (None, "native"), (None, "unsubmitted")):
            with self.subTest(head=head, wait=wait):
                self.reset()
                _, digest, _ = self.checkpoint()
                self.removal(digest, status="unavailable")
                self.removal(digest, operation=OTHER_REMOVAL, sequence=2, status=head, wait=wait)
                closure = self.scan()["workspace"]
                self.assertEqual(object_at(closure, CHECKPOINT, "checkpoint")["removals"], [
                    {"sequence": 1, "operation_id": REMOVAL, "status": "unavailable"},
                    {"sequence": 2, "operation_id": OTHER_REMOVAL, "status": head}])
                self.assertEqual(object_at(closure, OTHER_REMOVAL)["mode"],
                                 "terminated" if wait == "native" else "unsubmitted")
        with self.subTest("sole unsubmitted head"):
            self.reset()
            _, digest, _ = self.checkpoint()
            self.removal(digest, status=None, wait="unsubmitted")
            self.assertEqual(object_at(self.scan(), "workspace", CHECKPOINT, "checkpoint")["removals"],
                             [{"sequence": 1, "operation_id": REMOVAL, "status": None}])

    def test_receiptless_head_needs_independent_lifetime_closure(self) -> None:
        with self.subTest("missing Wait"):
            _, digest, _ = self.checkpoint()
            self.removal(digest, status="unavailable")
            self.removal(digest, operation=OTHER_REMOVAL, sequence=2, status=None, wait="missing")
            with self.assertRaisesRegex(AcceptanceFailure, "worker Wait is missing"):
                self.scan()
        with self.subTest("live owner"):
            self.reset()
            _, digest, _ = self.checkpoint()
            self.removal(digest, status="unavailable")
            head, _ = self.removal(digest, operation=OTHER_REMOVAL, sequence=2, status=None)
            lock = head / ".lifetime-owner.lock"
            lock.write_bytes(b"")
            with lock.open("r+b") as held:
                fcntl.flock(held.fileno(), fcntl.LOCK_EX)
                with self.assertRaisesRegex(AcceptanceFailure, "lifetime owner is still active"):
                    self.scan()
                fcntl.flock(held.fileno(), fcntl.LOCK_UN)

    def test_legacy_receipt_survives_previous_boot_absence(self) -> None:
        for path in ("creation", "removal"):
            with self.subTest(path):
                self.reset()
                lifetime, receipt = self.receipt_journal(path)
                before = receipt.read_bytes()
                self.assertEqual(self.receipt_status(path), "unavailable")
                self.publish_previous_boot_absence(lifetime)
                self.assertEqual(self.receipt_status(path), "unavailable")
                self.assertEqual(receipt.read_bytes(), before)
                self.assertEqual(object_at(self.scan(), "workspace", lifetime.name)["mode"], "recovered")

    def test_recovered_receipt_after_previous_boot_absence_stays_valid(self) -> None:
        for path in ("creation", "removal"):
            with self.subTest(path):
                self.reset()
                self.receipt_journal(path, wait="previous-boot", legacy=False)
                self.assertEqual(self.receipt_status(path), "unavailable")

    def test_legacy_receipt_refusals_after_absence(self) -> None:
        cases: dict[str, tuple[ReceiptJournalOptions, dict[str, str] | None, str]] = {
            "wrong termination digest": (
                {"termination": "9" * 64}, {}, "closure does not match"),
            "wrong termination cause": ({"cause": "completed"}, {}, "closure does not match"),
            "malformed absence": ({}, {"observed_boot": BOOT}, "absence witness mismatch"),
            "recovered success": (
                {"wait": "previous-boot", "legacy": False, "success": True}, None,
                "success has no native worker Wait"),
            "unregistered lifetime": (
                {"wait": "unregistered", "termination": "9" * 64}, None, "closure does not match"),
        }
        for path in ("creation", "removal"):
            for name, (journal, publish, reason) in cases.items():
                with self.subTest(path=path, case=name):
                    self.reset()
                    lifetime, _ = self.receipt_journal(path, **journal)
                    if publish is not None:
                        self.publish_previous_boot_absence(lifetime, **publish)
                    with self.assertRaisesRegex(AcceptanceFailure, reason):
                        self.scan()
            with self.subTest(path=path, case="native Wait after legacy"):
                self.reset()
                lifetime, _ = self.receipt_journal(path)
                native = lifetime_records(lifetime.name, sha((lifetime / "request.json").read_bytes()))
                write_record(lifetime / "lifetime-worker-wait.json", native["lifetime-worker-wait.json"])
                with self.assertRaisesRegex(AcceptanceFailure, "closure does not match"):
                    self.scan()
            with self.subTest(path=path, case="legacy success"):
                self.reset()
                with self.assertRaisesRegex(AcceptanceFailure, "success lacks native closure encoding"):
                    self.receipt_journal(path, success=True)

    def test_legacy_receipt_with_live_owner_is_refused(self) -> None:
        for path in ("creation", "removal"):
            with self.subTest(path):
                self.reset()
                lifetime, _ = self.receipt_journal(path)
                self.publish_previous_boot_absence(lifetime)
                lock = lifetime / ".lifetime-owner.lock"
                lock.write_bytes(b"")
                with lock.open("r+b") as held:
                    fcntl.flock(held.fileno(), fcntl.LOCK_EX)
                    with self.assertRaisesRegex(AcceptanceFailure, "lifetime owner is still active"):
                        self.scan()
                    fcntl.flock(held.fileno(), fcntl.LOCK_UN)

    def test_checkpoint_records_are_refused_in_other_kinds(self) -> None:
        for kind in ("capture", "checkpoint-remove"):
            with self.subTest(kind):
                self.reset()
                request = go_request(kind) if kind == "capture" else removal_request(
                    CHECKPOINT, "f" * 64, root=ROOT)
                request.update(root_id=ROOT)
                directory, _ = self.journal(request)
                write_record(directory / records.CHECKPOINT_CANDIDATE_RECORD, {})
                with self.assertRaises(AcceptanceFailure):
                    validate_managed_workspace_operation_closure(directory, ROOT)

    def orphan_receipt(self, digest: str) -> None:
        _, removal_digest = self.removal(digest, status=None)
        reference = attempt(CHECKPOINT, 2, REMOVAL, removal_digest)
        write_record(self.operations / CHECKPOINT / records.checkpoint_removal_name("receipt", 2), {
            "schema_version": 1, "attempt": reference, "status": "unavailable",
            "reason_code": "checkpoint_removal_failed", "termination_sha256": "f" * 64,
            "worker_wait_sha256": "0" * 64})

    def duplicate_attempt(self, digest: str) -> None:
        _, removal_digest = self.removal(digest, status=None)
        write_record(self.operations / CHECKPOINT / records.checkpoint_removal_name("attempt", 2),
                     attempt(CHECKPOINT, 2, REMOVAL, removal_digest))

    def reset(self) -> None:
        """Remove every journal this test wrote, keeping the owned state root."""
        if self.operations.exists():
            shutil.rmtree(self.operations)

    @staticmethod
    def read(path: Path) -> dict[str, object]:
        return _read_control_record(path, "fixture record")

if __name__ == "__main__":
    unittest.main()
