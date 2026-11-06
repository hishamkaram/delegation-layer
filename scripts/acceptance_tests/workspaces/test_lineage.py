"""Policy and fault tests for the managed-workspace lineage scenario."""

from __future__ import annotations

from copy import deepcopy
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from scripts.acceptance_tests.record_fixtures import array_at, object_at
from unittest import mock

import scripts.delegate_acceptance.workspaces.scenarios.application as application
import scripts.delegate_acceptance.workspaces.scenarios.lineage as lineage
import scripts.delegate_acceptance.workspaces.scenarios.lifecycle as lifecycle
import scripts.delegate_acceptance.workspaces.scenarios.operations as scenario_operations
import scripts.delegate_acceptance.workspaces.scenarios.workspace_operations as scenario_workspace_operations
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.workspaces.scenarios.context import WorkspaceContext
from scripts.delegate_acceptance.workspaces.scenarios.evidence import ReceiptFinalizer
from scripts.delegate_acceptance.workspaces.scenarios.lifecycle import SuccessfulTaskEvidence
from scripts.delegate_acceptance.workspaces.scenarios.source import _git_executable
from scripts.delegate_acceptance.workspaces.scenarios.model import CaptureEvidence, WorkspaceTask

ROOT_ID = "e" * 32
PARENT_ID = "a" * 32
CHILD_ID = "b" * 32
FIRST_OPERATION = "1" * 32
SECOND_OPERATION = "2" * 32
BASE_OID = "3" * 40
HEAD_OID = "4" * 40


def _task_fixture(task_id: str) -> WorkspaceTask:
    return {"task_id": task_id, "workspace_id": task_id, "root_id": ROOT_ID,
            "workspace": {"workspace": {"id": task_id}}}


def _capture_evidence(operation_id: str, workspace_id: str = CHILD_ID) -> CaptureEvidence:
    return {"operation_id": operation_id, "workspace_id": workspace_id, "root_id": ROOT_ID,
            "base_oid": BASE_OID, "observed_head": HEAD_OID, "tree_oid": "6" * 40,
            "snapshot_ref": "refs/delegate/" + ROOT_ID + "/workspaces/" + workspace_id
                            + "/captures/" + operation_id,
            "subject": {"kind": "task", "id": workspace_id, "sha256": "d" * 64}, "artifacts": []}


def _write_capture(
    state: Path, operation_id: str, workspace_id: str, tree: str,
) -> dict[str, object]:
    artifacts = state / "workspace-operations" / operation_id / "artifacts"
    artifacts.mkdir(parents=True, exist_ok=True)
    patch = ("diff --git a/" + operation_id + " b/" + operation_id + "\n").encode()
    (artifacts / "diff.patch").write_bytes(patch)
    name_status = b"M\x00tracked.txt\x00"
    (artifacts / "name-status.z").write_bytes(name_status)
    return {
        "status": "captured",
        "workspace_id": workspace_id,
        "operation_id": operation_id,
        "subject": {"kind": "task", "id": CHILD_ID, "sha256": "d" * 64},
        "termination_sha256": "4" * 64,
        "worker_wait_sha256": "5" * 64,
        "candidate": {
            "workspace_id": workspace_id,
            "operation_id": operation_id,
            "subject": {"kind": "task", "id": CHILD_ID, "sha256": "d" * 64},
            "base_oid": BASE_OID,
            "observed_head": HEAD_OID,
            "tree_oid": tree,
            "snapshot_ref": "refs/delegate/" + ROOT_ID + "/workspaces/" + workspace_id
                            + "/captures/" + operation_id,
            "object_format": "sha1",
        },
        "artifacts": [
            {"name": "diff.patch", "length": len(patch), "sha256": hashlib.sha256(patch).hexdigest()},
            {"name": "name-status.z", "length": len(name_status),
             "sha256": hashlib.sha256(name_status).hexdigest()},
        ],
    }


class TaskObservationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.state = Path(self.directory.name)
        (self.state / "root.json").write_text(json.dumps({"root_id": ROOT_ID}))
        self.first = _write_capture(self.state, FIRST_OPERATION, CHILD_ID, "6" * 40)
        self.second = _write_capture(self.state, SECOND_OPERATION, CHILD_ID, "7" * 40)
        self.expected = lineage.verify_capture_receipt(self.state, self.first)

    def _response(self, capture, state="busy", **extra):
        workspace = {"id": CHILD_ID, "state": state}
        if capture is not None:
            workspace["capture"] = capture
        return {"workspace": workspace, **extra}

    def test_observation_binds_to_the_tasks_own_capture_while_workspace_is_busy(self):
        observed = lineage.verify_task_observation(
            self.state, self._response(self.first), self.expected, expected_state="busy",
        )
        self.assertEqual(observed["operation_id"], FIRST_OPERATION)
        self.assertEqual(observed["workspace_state"], "busy")

    def test_latest_workspace_capture_is_not_the_first_tasks_capture(self):
        with self.assertRaisesRegex(RuntimeError, "points to capture " + SECOND_OPERATION):
            lineage.verify_task_observation(
                self.state, self._response(self.second, state="retained"), self.expected,
            )

    def test_missing_capture_is_a_failure_not_a_pass(self):
        with self.assertRaisesRegex(RuntimeError, "omitted its workspace capture"):
            lineage.verify_task_observation(
                self.state, self._response(None), self.expected,
            )

    def test_busy_workspace_cannot_be_reported_as_retained(self):
        with self.assertRaisesRegex(RuntimeError, "reports workspace 'retained', not busy"):
            lineage.verify_task_observation(
                self.state, self._response(self.first, state="retained"), self.expected,
                expected_state="busy",
            )

    def test_continuation_requires_a_retained_workspace(self):
        offer = {"continuation": {"resumable": True}}
        with self.assertRaisesRegex(RuntimeError, "advertises continuation while workspace is 'busy'"):
            lineage.verify_task_observation(
                self.state, self._response(self.first, **offer), self.expected,
            )
        observed = lineage.verify_task_observation(
            self.state, self._response(self.first, state="retained", **offer), self.expected,
        )
        self.assertEqual(observed["workspace_state"], "retained")

    def test_tampered_artifact_bytes_fail_the_observation(self):
        patch = self.state / "workspace-operations" / FIRST_OPERATION / "artifacts" / "diff.patch"
        patch.write_bytes(b"x" * patch.stat().st_size)
        with self.assertRaisesRegex(RuntimeError, "artifact digest"):
            lineage.verify_task_observation(
                self.state, self._response(self.first), self.expected,
            )


class CaptureInventoryTests(unittest.TestCase):
    """The capture inventory is the exact Go contract: cumulative pair plus the observed-turn pair."""

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.state = Path(self.directory.name)
        (self.state / "root.json").write_text(json.dumps({"root_id": ROOT_ID}))
        self.capture = _write_capture(self.state, FIRST_OPERATION, CHILD_ID, "6" * 40)
        self.patch, self.names = [object_at(item) for item in array_at(self.capture, "artifacts")]
        self.root = self.state / "workspace-operations" / FIRST_OPERATION / "artifacts"

    def _turn_pair(self):
        pair = []
        for name in ("turn-diff.patch", "turn-name-status.z"):
            data = name.encode() + b"\n"
            (self.root / name).write_bytes(data)
            pair.append({"name": name, "length": len(data), "sha256": hashlib.sha256(data).hexdigest()})
        return pair

    def _verify(self, artifacts, turn=None):
        capture = {**self.capture, "artifacts": artifacts}
        if turn is not None:
            capture["candidate"] = {**object_at(capture, "candidate"), "turn": turn}
        return lineage.verify_capture_receipt(self.state, capture)

    def test_cumulative_pair_is_required_with_or_without_a_turn(self):
        observed = {"status": "observed"}
        for artifacts, turn in (([self.patch], None), ([self.names], None), ([], None),
                                ([self.patch, *self._turn_pair()], observed),
                                (self._turn_pair(), observed)):
            with self.subTest([item["name"] for item in artifacts]):
                with self.assertRaisesRegex(RuntimeError, "cumulative artifact|nonempty patch"):
                    self._verify(artifacts, turn)
        for artifacts in ([self.patch], [self.names]):
            with self.assertRaisesRegex(RuntimeError, "omitted a cumulative artifact"):
                self._verify(artifacts)
        names = {item["name"] for item in self._verify([self.patch, self.names])["artifacts"]}
        self.assertEqual(names, {"diff.patch", "name-status.z"})

    def test_observed_turn_requires_the_complete_turn_pair_and_others_forbid_it(self):
        pair = self._turn_pair()
        base = [self.patch, self.names]
        self.assertEqual(len(self._verify([*base, *pair], {"status": "observed"})["artifacts"]), 4)
        for turn in ({"status": "unavailable"}, None):
            self._verify(base, turn)
            with self.assertRaisesRegex(RuntimeError, "turn artifacts"):
                self._verify([*base, *pair], turn)
        for partial in (pair[:1], pair[1:]):
            with self.assertRaisesRegex(RuntimeError, "turn artifacts"):
                self._verify([*base, *partial], {"status": "observed"})
        with self.assertRaisesRegex(RuntimeError, "turn artifacts"):
            self._verify(base, {"status": "observed"})

    def test_unknown_or_duplicated_artifact_names_are_rejected(self):
        (self.root / "extra.txt").write_bytes(b"x")
        extra = {"name": "extra.txt", "length": 1, "sha256": hashlib.sha256(b"x").hexdigest()}
        with self.assertRaisesRegex(RuntimeError, "allowlisted"):
            self._verify([self.patch, self.names, extra])
        with self.assertRaisesRegex(RuntimeError, "duplicated"):
            self._verify([self.patch, self.names, self.names])

    def test_limit_omission_counts_as_present_and_matches_the_go_descriptor(self):
        omitted = {"name": "name-status.z", "omitted": "limit", "length": 0}
        for variant in (omitted, {**omitted, "sha256": ""}, {**omitted, "truncated": False}):
            evidence = self._verify([self.patch, variant])
            self.assertIn({"name": "name-status.z", "omitted": "limit"}, evidence["artifacts"])
        turn_omitted = {**omitted, "name": "turn-diff.patch"}
        pair = self._turn_pair()
        self._verify([self.patch, self.names, turn_omitted, pair[1]], {"status": "observed"})

    def test_omission_must_be_an_empty_limit_descriptor(self):
        omitted = {"name": "name-status.z", "omitted": "limit", "length": 0}
        invalid = {
            "other reason": ({**omitted, "omitted": "oversize"}, "empty limit descriptor"),
            "nonzero length": ({**omitted, "length": 1}, "empty limit descriptor"),
            "digest present": ({**omitted, "sha256": "a" * 64}, "empty limit descriptor"),
            "digest not a string": ({**omitted, "sha256": 5}, "empty limit descriptor"),
            "truncated": ({**omitted, "truncated": True}, "truncated"),
            "truthy truncated": ({**omitted, "truncated": 1}, "truncated"),
            "reason not a string": ({**omitted, "omitted": 5}, "omitted reason"),
            "length missing": ({"name": "name-status.z", "omitted": "limit"}, "length is invalid"),
            "length boolean": ({**omitted, "length": False}, "length is invalid"),
        }
        for label, (descriptor, message) in invalid.items():
            with self.subTest(label), self.assertRaisesRegex(RuntimeError, message):
                self._verify([self.patch, descriptor])

    def test_complete_artifacts_are_never_truncated(self):
        for flag in (True, 1, "false"):
            with self.subTest(flag), self.assertRaisesRegex(RuntimeError, "truncated"):
                self._verify([self.patch, {**self.names, "truncated": flag}])
        self._verify([self.patch, {**self.names, "truncated": False}])


class FreshTaskTurnTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.state = Path(directory.name)
        (self.state / "root.json").write_text(json.dumps({"root_id": ROOT_ID}))
        self.capture = _write_capture(self.state, FIRST_OPERATION, CHILD_ID, "6" * 40)
        self.context = mock.Mock(spec=WorkspaceContext)
        self.context.state, self.context.source = self.state, self.state / "source"
        self.task = {"task_id": CHILD_ID, "root_id": ROOT_ID,
                     "workspace": {"id": CHILD_ID, "reservation": 17}}
        self.task_path = self.state / "tasks" / CHILD_ID / "task.json"
        self.task_path.parent.mkdir(parents=True)
        self._write_task(self.task)
        self.reference = ("refs/delegate/" + ROOT_ID + "/workspaces/" + CHILD_ID
                          + "/baselines/00000000000000000017")
        self.turn = {"status": "observed", "baseline_ref": self.reference,
                     "start_tree_oid": "7" * 40, "content": "changed",
                     "head": "unchanged", "index": "changed"}
        object_at(self.capture, "candidate")["turn"] = self.turn
        self._artifact("turn-diff.patch", b"diff --git a/new b/new\n")
        self._artifact("turn-name-status.z", b"A\x00" + lineage.REUSED_FILE.encode() + b"\x00")

    def _write_task(self, record):
        self.task_path.write_text(json.dumps(record))

    def _artifact(self, name, data):
        root = self.state / "workspace-operations" / FIRST_OPERATION / "artifacts"
        (root / name).write_bytes(data)
        self.capture["artifacts"] = [item for item in array_at(self.capture, "artifacts") if object_at(item)["name"] != name]
        array_at(self.capture, "artifacts").append({"name": name, "length": len(data),
                                           "sha256": hashlib.sha256(data).hexdigest()})

    def _verify(self, capture=None):
        with mock.patch.object(scenario_workspace_operations, "_git_ok",
                               return_value=b"7" * 40 + b"\n") as git:
            evidence = scenario_workspace_operations.verify_task_turn(
                self.context, self.capture if capture is None else capture, task_id=CHILD_ID,
                expected_files=frozenset({lineage.REUSED_FILE}),
                forbidden_files=frozenset({lineage.FIXTURE_FILE}))
        return evidence, git

    def test_fresh_turn_binds_the_real_reservation_and_supervised_tree_lookup(self):
        evidence, git = self._verify()
        self.assertEqual(evidence["turn"], self.turn)
        git.assert_called_once_with(
            self.context, ["-C", str(self.context.source), "rev-parse", "--verify",
                           self.reference + "^{tree}"],
            self.context.source, label="turn-baseline-tree")

    def test_missing_and_unavailable_turns_remain_generic_but_fail_fresh_acceptance(self):
        for turn in (None, {"status": "unavailable", "reason_code": "baseline_unavailable"}):
            capture = deepcopy(self.capture)
            object_at(capture, "candidate")["turn"] = turn
            capture["artifacts"] = array_at(capture, "artifacts")[:2]
            with self.subTest(turn=turn):
                lineage.verify_capture_receipt(self.state, capture)
                with self.assertRaisesRegex(RuntimeError, "not observed"):
                    self._verify(capture)

    def test_observation_fields_and_reason_are_required(self):
        cases = {"status": "unavailable", "reason_code": "baseline_unavailable",
                 "content": "", "head": "unknown", "index": None}
        for field, value in cases.items():
            capture = deepcopy(self.capture)
            object_at(capture, "candidate", "turn")[field] = value
            with self.subTest(field=field), self.assertRaises(RuntimeError):
                self._verify(capture)

    def test_task_subject_kind_id_and_receipt_must_match(self):
        for subject in ({"kind": "operation", "id": CHILD_ID},
                        {"kind": "task", "id": PARENT_ID}):
            capture = deepcopy(self.capture)
            object_at(capture, "candidate", "subject").update(subject)
            object_at(capture, "subject").update(subject)
            with self.subTest(subject), self.assertRaisesRegex(RuntimeError, "exact task subject"):
                self._verify(capture)
        capture = deepcopy(self.capture)
        object_at(capture, "subject")["id"] = PARENT_ID
        with self.assertRaisesRegex(RuntimeError, "exact task subject"):
            self._verify(capture)

    def test_task_root_workspace_and_reservation_are_authoritative(self):
        records = [
            {**self.task, "root_id": "f" * 32}, {**self.task, "task_id": PARENT_ID},
            {**self.task, "workspace": None},
            {**self.task, "workspace": {"id": PARENT_ID, "reservation": 17}},
        ]
        for sequence in (None, False, 0, -1, "17", 2**64, 18):
            records.append({**self.task, "workspace": {"id": CHILD_ID, "reservation": sequence}})
        for record in records:
            self._write_task(record)
            with self.subTest(record), self.assertRaises(RuntimeError):
                self._verify()
        self.task_path.unlink()
        with self.assertRaisesRegex(RuntimeError, "task request record is absent"):
            self._verify()

    def test_baseline_ref_cannot_be_missing_or_name_another_root_workspace_or_reservation(self):
        for reference in (None, "refs/heads/main", self.reference[:-2] + "18",
                          self.reference.replace(ROOT_ID, "f" * 32),
                          self.reference.replace(CHILD_ID, PARENT_ID),
                          self.reference.rstrip("17") + "17/extra"):
            capture = deepcopy(self.capture)
            object_at(capture, "candidate", "turn")["baseline_ref"] = reference
            with self.subTest(reference), self.assertRaisesRegex(RuntimeError, "task reservation"):
                self._verify(capture)

    def test_start_tree_must_be_valid_and_match_the_pinned_tree(self):
        for tree in (None, "", "bad-oid", "8" * 40):
            capture = deepcopy(self.capture)
            object_at(capture, "candidate", "turn")["start_tree_oid"] = tree
            with self.subTest(tree), self.assertRaisesRegex(RuntimeError, "start tree"):
                self._verify(capture)
        with mock.patch.object(scenario_workspace_operations, "_git_ok", side_effect=RuntimeError("missing ref")):
            with self.assertRaisesRegex(RuntimeError, "missing ref"):
                scenario_workspace_operations.verify_task_turn(
                    self.context, self.capture, task_id=CHILD_ID, expected_files=frozenset())

    def test_turn_artifacts_must_be_present_complete_and_match_their_hashed_bytes(self):
        for name in ("turn-diff.patch", "turn-name-status.z"):
            capture = deepcopy(self.capture)
            capture["artifacts"] = [item for item in array_at(capture, "artifacts") if object_at(item)["name"] != name]
            with self.subTest(name=name, fault="missing"), self.assertRaisesRegex(RuntimeError, "turn artifacts"):
                self._verify(capture)
            array_at(capture, "artifacts").append({"name": name, "omitted": "limit", "length": 0})
            with self.subTest(name=name, fault="omitted"), self.assertRaisesRegex(RuntimeError, "were omitted"):
                self._verify(capture)
            capture = deepcopy(self.capture)
            artifact = object_at(next(item for item in array_at(capture, "artifacts") if object_at(item)["name"] == name))
            length = artifact["length"]
            if type(length) is not int:
                self.fail("fixture artifact length is not an integer")
            for field, value in (("sha256", "0" * 64), ("length", length + 1)):
                corrupt = deepcopy(capture)
                object_at(next(item for item in array_at(corrupt, "artifacts") if object_at(item)["name"] == name))[field] = value
                with self.subTest(name=name, fault=field), self.assertRaisesRegex(RuntimeError, "artifact digest"):
                    self._verify(corrupt)

    def test_expected_files_and_previous_turn_exclusion_are_checked_on_turn_bytes(self):
        for data, message in (
            (b"A\x00other.txt\x00", "omitted expected files"),
            (b"", "omitted expected files"),
            (b"A\x00" + lineage.REUSED_FILE.encode() + b"\x00A\x00"
             + lineage.FIXTURE_FILE.encode() + b"\x00", "forbidden previous-turn"),
        ):
            self._artifact("turn-name-status.z", data)
            with self.subTest(data), self.assertRaisesRegex(RuntimeError, message):
                self._verify()

    def test_name_status_is_nul_safe_and_malformed_pairs_fail(self):
        self._artifact("turn-name-status.z", b"A\x00" + lineage.REUSED_FILE.encode()
                       + b"\x00M\x00odd\n\t\xff.txt\x00")
        self._verify()
        for data in (b"A\x00path", b"A\x00", b"A\x00\x00", b"?\x00x\x00",
                     b"R100\x00a\x00b\x00", b"A\x00a\x00M\x00a\x00"):
            self._artifact("turn-name-status.z", data)
            with self.subTest(data), self.assertRaisesRegex(RuntimeError, "turn name-status"):
                self._verify()

    def test_files_cannot_be_reported_with_unchanged_content(self):
        self.turn["content"] = "unchanged"
        with self.assertRaisesRegex(RuntimeError, "files require changed content"):
            self._verify()

    def test_operation_capture_keeps_its_generic_historical_contract(self):
        for turn in (None, {"status": "unavailable"}):
            capture = deepcopy(self.capture)
            object_at(capture, "candidate", "subject")["kind"] = "operation"
            object_at(capture, "subject")["kind"] = "operation"
            object_at(capture, "candidate")["turn"] = turn
            capture["artifacts"] = array_at(capture, "artifacts")[:2]
            evidence = lineage.verify_capture_receipt(self.state, capture)
            self.assertEqual(object_at(evidence, "subject")["kind"], "operation")


class CheckoutIdentityTests(unittest.TestCase):
    def _facts(self, identifier, path, ref, base=BASE_OID):
        return lineage.CheckoutFacts(identifier, path, base, ref)

    def test_workspace_json_must_publish_path_and_owned_base_pin(self):
        workspace: dict[str, object] = {"id": PARENT_ID, "path": "/c/a/checkout", "base_oid": BASE_OID,
                     "base_ref": "refs/delegate/" + ROOT_ID + "/workspaces/" + PARENT_ID + "/base"}
        self.assertEqual(lineage.checkout_facts(workspace).base_ref, workspace["base_ref"])
        for key in ("path", "base_oid", "base_ref"):
            with self.subTest(missing=key), self.assertRaises(RuntimeError):
                lineage.checkout_facts({k: v for k, v in workspace.items() if k != key})
        with self.assertRaises(RuntimeError):
            lineage.checkout_facts({**workspace, "base_ref": "refs/heads/main"})

    def test_sibling_requires_distinct_identity_pin_and_unnested_paths(self):
        container = Path("/c")
        parent = self._facts(PARENT_ID, "/c/a/checkout", "refs/delegate/r/a/base")
        child = self._facts(CHILD_ID, "/c/b/checkout", "refs/delegate/r/b/base")
        lineage.require_sibling_checkouts(parent, child, container)
        cases = {
            "identity": self._facts(PARENT_ID, "/c/b/checkout", child.base_ref),
            "pin": self._facts(CHILD_ID, "/c/b/checkout", parent.base_ref),
            "base": self._facts(CHILD_ID, "/c/b/checkout", child.base_ref, base="9" * 40),
            "container": self._facts(CHILD_ID, "/elsewhere/checkout", child.base_ref),
            "nested": self._facts(CHILD_ID, "/c/a/checkout/inner", child.base_ref),
        }
        for name, candidate in cases.items():
            with self.subTest(name), self.assertRaises(RuntimeError):
                lineage.require_sibling_checkouts(parent, candidate, container)


class SharedHelperTests(unittest.TestCase):
    """The lineage scenario runs on the canonical dispatch, capture-wait and exec owners."""

    def _dispatch_context(self):
        context = mock.Mock(spec=WorkspaceContext)
        context.state, context.source = Path("/state"), Path("/source")
        context.workspace_budget = "5m"
        context.source_baseline = {"head": BASE_OID}
        context.command_lock = mock.MagicMock()
        context.attempted_task_ids = []
        return context

    def _dispatch_arguments(self, context, **options):
        response = {"task_id": CHILD_ID, "root_id": ROOT_ID, "task_record": "created",
                    "admission": "admitted", "workspace": {"id": PARENT_ID, "state": "busy"}}

        def control(_context, _arguments, **kwargs):
            return (0, response) if kwargs["label"] == "dispatch" else (0, {})

        with (
            mock.patch.object(scenario_operations.uuid, "uuid4", return_value=mock.Mock(hex=CHILD_ID)),
            mock.patch.object(scenario_operations, "_control", side_effect=control) as controlled,
        ):
            task = scenario_operations._dispatch(
                context, Path("/brief"), task_budget="30s", **options)
        self.assertEqual(task["task_id"], CHILD_ID)
        self.assertEqual(context.attempted_task_ids, [CHILD_ID])
        context.add_task.assert_called_once_with(CHILD_ID)
        return controlled.call_args_list[0].args[1]

    def test_scenario_binds_the_canonical_helpers_without_private_copies(self):
        self.assertIs(lineage._dispatch, scenario_operations._dispatch)
        self.assertIs(lineage._wait_capture, scenario_operations._wait_capture)
        self.assertIs(lineage._workspace_exec, scenario_workspace_operations._workspace_exec)
        for removed in ("_dispatch_child", "_wait_subject_capture", "_exec_script",
                        "child_dispatch_arguments"):
            self.assertFalse(hasattr(lineage, removed), removed)

    def test_fresh_dispatch_defaults_to_the_source_and_baseline(self):
        arguments = self._dispatch_arguments(self._dispatch_context(), fresh=True)
        self.assertEqual(arguments[arguments.index("--cwd") + 1], "/source")
        self.assertEqual(arguments[arguments.index("--base") + 1], BASE_OID)
        self.assertEqual(arguments[arguments.index("--isolation") + 1], "git-worktree")

    def test_fresh_dispatch_can_start_from_a_managed_parent_checkout(self):
        arguments = self._dispatch_arguments(
            self._dispatch_context(), fresh=True, source="/c/a/checkout", base="9" * 40)
        self.assertEqual(arguments[arguments.index("--cwd") + 1], "/c/a/checkout")
        self.assertEqual(arguments[arguments.index("--base") + 1], "9" * 40)
        self.assertNotIn("--workspace", arguments)

    def test_workspace_reuse_without_identity_fails_before_recording_an_attempt(self):
        context = self._dispatch_context()
        with mock.patch.object(scenario_operations, "_control") as controlled:
            with self.assertRaisesRegex(RuntimeError, "workspace reuse dispatch lacks ID"):
                scenario_operations._dispatch(context, Path("/brief"), task_budget="30s", fresh=False)
        controlled.assert_not_called()
        self.assertEqual(context.attempted_task_ids, [])

    def test_workspace_reuse_cannot_override_source_or_base(self):
        for override in ({"source": "/c/a/checkout"}, {"base": BASE_OID}):
            context = self._dispatch_context()
            with self.subTest(override), mock.patch.object(scenario_operations, "_control") as controlled:
                with self.assertRaisesRegex(RuntimeError, "cannot override"):
                    scenario_operations._dispatch(
                        context, Path("/brief"), task_budget="30s", fresh=False,
                        workspace_id=PARENT_ID, **override)
                controlled.assert_not_called()
            self.assertEqual(context.attempted_task_ids, [])

    def _wait_capture_with(self, captures, operation=None, **options):
        workspace = {"id": PARENT_ID, "state": "retained"}
        if operation is not None:
            workspace["operation"] = operation
        responses = [{"workspace": {**workspace, "capture": capture}} for capture in captures]
        context = mock.Mock(spec=WorkspaceContext)
        context.state = Path("/state")
        with (
            mock.patch.object(scenario_operations, "_wait_workspace", side_effect=responses),
            mock.patch.object(scenario_operations.time, "sleep"),
        ):
            return scenario_operations._wait_capture(
                context, {"workspace_id": PARENT_ID, "task_id": CHILD_ID}, **options)

    @staticmethod
    def _capture(subject_id, status="captured", **fields):
        return {"status": status, "subject": {"kind": "task", "id": subject_id}, **fields}

    def _unavailable_failure(self, control, operation=None, **fields):
        """Run one unavailable capture of the task's own subject; return the error and the control mock."""
        capture = self._capture(CHILD_ID, status="unavailable", **{
            "workspace_id": PARENT_ID, "operation_id": FIRST_OPERATION,
            "reason_code": "capture_failed", **fields})
        with mock.patch.object(scenario_operations, "_control", control):
            with self.assertRaises(RuntimeError) as raised:
                self._wait_capture_with([capture], operation=operation)
        return str(raised.exception), control

    @staticmethod
    def _operation_response(operation_id, status="failed", reason="git_executable_unavailable"):
        return (0, {"workspace": {"id": PARENT_ID, "state": "retained", "operation": {
            "operation_id": operation_id, "status": status, "reason_code": reason}}})

    def test_omitted_subject_means_the_task_not_the_latest_capture_of_a_reused_workspace(self):
        previous, own = self._capture(PARENT_ID), self._capture(CHILD_ID)
        _response, capture = self._wait_capture_with([previous, previous, own])
        self.assertIs(capture, own)

    def test_omitted_subject_ignores_another_turns_unavailable_capture(self):
        previous = self._capture(PARENT_ID, status="unavailable")
        own = self._capture(CHILD_ID)
        _response, capture = self._wait_capture_with([previous, own])
        self.assertIs(capture, own)
        with self.assertRaisesRegex(RuntimeError, "became unavailable"):
            self._wait_capture_with([self._capture(CHILD_ID, status="unavailable")])

    def test_unavailable_capture_reports_the_operations_public_reason_with_one_status_query(self):
        control = mock.Mock(return_value=self._operation_response(FIRST_OPERATION))
        message, control = self._unavailable_failure(control)
        for fact in ("workspace " + PARENT_ID, "subject " + CHILD_ID,
                     "capture_operation " + FIRST_OPERATION, "capture_reason capture_failed",
                     "operation_status failed", "operation_reason git_executable_unavailable"):
            self.assertIn(fact, message)
        self.assertTrue(message.startswith("workspace capture became unavailable"))
        control.assert_called_once()
        arguments = control.call_args.args[1]
        self.assertEqual(arguments[arguments.index("--operation") + 1], FIRST_OPERATION)
        self.assertEqual(arguments[arguments.index("status") + 1], PARENT_ID)
        self.assertNotIn("--watch", arguments)

    def test_unavailable_capture_uses_the_observed_operation_without_another_query(self):
        operation = {"operation_id": FIRST_OPERATION, "status": "failed",
                     "reason_code": "git_executable_unavailable"}
        message, control = self._unavailable_failure(mock.Mock(), operation=operation)
        self.assertIn("operation_reason git_executable_unavailable", message)
        control.assert_not_called()

    def test_unavailable_capture_does_not_attribute_another_operations_reason(self):
        control = mock.Mock(return_value=self._operation_response(SECOND_OPERATION))
        message, _control = self._unavailable_failure(control)
        self.assertIn("operation_absent", message)
        self.assertNotIn("git_executable_unavailable", message)
        observed = {"operation_id": SECOND_OPERATION, "status": "failed",
                    "reason_code": "git_executable_unavailable"}
        message, control = self._unavailable_failure(
            mock.Mock(return_value=self._operation_response(SECOND_OPERATION)), operation=observed)
        self.assertNotIn("git_executable_unavailable", message)
        control.assert_called_once()

    def test_unavailable_capture_survives_any_failed_operation_lookup_without_raw_output(self):
        secret = "SECRET-TOKEN /private/path"
        for kind in (RuntimeError, TypeError, Exception):
            with self.subTest(kind.__name__):
                control = mock.Mock(side_effect=kind("delegate command failed: stdout=" + secret))
                message, _control = self._unavailable_failure(control)
                self.assertTrue(message.startswith("workspace capture became unavailable"))
                self.assertIn("capture_reason capture_failed", message)
                self.assertIn("operation_lookup_failed:" + kind.__name__, message)
                self.assertNotIn("SECRET-TOKEN", message)
                self.assertNotIn("/private/path", message)

    def test_operation_lookup_does_not_swallow_an_interrupt(self):
        capture = self._capture(CHILD_ID, status="unavailable", operation_id=FIRST_OPERATION)
        with mock.patch.object(scenario_operations, "_control", side_effect=KeyboardInterrupt):
            with self.assertRaises(KeyboardInterrupt):
                self._wait_capture_with([capture])

    def test_unavailable_capture_renders_only_bounded_identifiers(self):
        control = mock.Mock(return_value=self._operation_response(
            FIRST_OPERATION, reason="/home/x/secret path"))
        message, _control = self._unavailable_failure(control, reason_code="Bad Reason\n" + "x" * 99)
        self.assertIn("capture_reason invalid", message)
        self.assertIn("operation_reason invalid", message)
        self.assertNotIn("/home/x", message)
        for malformed in ("not-hex", None):
            with self.subTest(malformed):
                message, control = self._unavailable_failure(mock.Mock(), operation_id=malformed)
                self.assertIn("capture_operation " + ("none" if malformed is None else "invalid"), message)
                control.assert_not_called()

    def test_another_subjects_unavailable_capture_does_not_query_the_operation(self):
        sibling = self._capture(PARENT_ID, status="unavailable", operation_id=SECOND_OPERATION,
                                reason_code="capture_failed")
        control = mock.Mock()
        with mock.patch.object(scenario_operations, "_control", control):
            _response, capture = self._wait_capture_with([sibling, self._capture(CHILD_ID)])
        self.assertEqual(capture["status"], "captured")
        control.assert_not_called()

    def test_explicit_operation_subject_selects_a_verification_capture(self):
        task_capture, exec_capture = self._capture(CHILD_ID), self._capture(FIRST_OPERATION)
        _response, capture = self._wait_capture_with(
            [task_capture, exec_capture], subject_id=FIRST_OPERATION)
        self.assertIs(capture, exec_capture)

    def test_selected_subject_ignores_another_subjects_unavailable_capture(self):
        other = self._capture(PARENT_ID, status="unavailable")
        own = self._capture(CHILD_ID)
        _response, capture = self._wait_capture_with([other, own], subject_id=CHILD_ID)
        self.assertIs(capture, own)
        with self.assertRaisesRegex(RuntimeError, "became unavailable"):
            self._wait_capture_with([self._capture(CHILD_ID, status="unavailable")],
                                    subject_id=CHILD_ID)

    def test_selected_subject_wait_expires_instead_of_accepting_another_capture(self):
        with mock.patch.object(scenario_operations.time, "monotonic", side_effect=[0, 1, 10**9]):
            with self.assertRaisesRegex(TimeoutError, "subject " + CHILD_ID):
                self._wait_capture_with([self._capture(PARENT_ID)], subject_id=CHILD_ID)

    def _exec_arguments(self, **options):
        pending = {"workspace": {"id": PARENT_ID, "state": "busy", "next_action": "watch_workspace",
                                 "operation_id": FIRST_OPERATION}}
        finished = {"workspace": {"id": PARENT_ID, "state": "retained", "operation": {
            "operation_id": FIRST_OPERATION, "status": "succeeded", "command_exit": 0}}}
        context = mock.Mock(spec=WorkspaceContext)
        context.state = Path("/state")
        with (
            mock.patch.object(scenario_workspace_operations, "_control",
                              return_value=(3, pending)) as controlled,
            mock.patch.object(scenario_workspace_operations, "_wait_workspace", return_value=finished),
        ):
            result = scenario_workspace_operations._workspace_exec(
                context, {"workspace_id": PARENT_ID, "task_id": CHILD_ID}, **options)
        return result, controlled.call_args.args[1], controlled.call_args.kwargs["label"]

    def test_exec_defaults_to_the_fixture_command(self):
        result, arguments, label = self._exec_arguments()
        separator = arguments.index("--")
        self.assertEqual(tuple(arguments[separator + 1:]),
                         scenario_workspace_operations.FIXTURE_EXEC_COMMAND)
        self.assertIn(lineage.FIXTURE_FILE, arguments[-1])
        self.assertEqual((label, result["operation_id"], result["command_exit"]),
                         ("workspace-exec", FIRST_OPERATION, 0))

    def test_exec_passes_one_literal_script_argument_and_its_label(self):
        script = "set -eu\nprintf '%s\\n' 'a b' > \"x y\"\n"
        result, arguments, label = self._exec_arguments(
            command=("/bin/sh", "-c", script), label="workspace-exec-lineage")
        self.assertEqual(arguments[arguments.index("--") + 1:], ["/bin/sh", "-c", script])
        self.assertEqual((label, result["operation_id"]), ("workspace-exec-lineage", FIRST_OPERATION))


class CommitAndDirtyScriptTests(unittest.TestCase):
    def test_script_leaves_a_committed_prefix_and_uncommitted_cumulative_edits(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            checkout = root / "checkout"
            checkout.mkdir()
            owner = DirectoryOwner()
            environment = {key: value for key, value in os.environ.items()
                           if not key.startswith("GIT_")}
            environment.update({"GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"})
            try:
                processes = Processes(root / "captures", environment=environment, owner=owner,
                                      environment_keys_only=True)
                git = _git_executable()

                def run(name, argv):
                    return processes.run(name, argv, checkout, stdin=b"").output()

                run("init", [git, "init", "--quiet"])
                (checkout / lineage.TRACKED_FILE).write_bytes(b"managed workspace source\n")
                run("add", [git, "add", "--", lineage.TRACKED_FILE])
                run("base", [git, "-c", "user.name=Base", "-c", "user.email=b@example.invalid",
                             "commit", "--quiet", "-m", "base"])
                base = run("base-oid", [git, "rev-parse", "HEAD"]).decode().strip()
                for name in (lineage.FIXTURE_FILE, lineage.REUSED_FILE):
                    (checkout / name).write_bytes(lineage.FINAL_TREE[name])
                run("intent", [git, "add", "-N", "--", lineage.FIXTURE_FILE, lineage.REUSED_FILE])
                run("script", ["/bin/sh", "-c", lineage.commit_and_dirty_script(str(git))])

                self.assertEqual(run("parent", [git, "rev-parse", "HEAD^"]).decode().strip(), base)
                committed = run("committed", [git, "ls-tree", "-r", "--name-only", "HEAD"]).decode().split()
                self.assertIn(lineage.INTERMEDIATE_FILE, committed)
                self.assertNotIn(lineage.UNTRACKED_FILE, committed)
                status = run("status", [git, "status", "--porcelain=v1", "--untracked-files=all"]).decode()
                self.assertIn(" M " + lineage.TRACKED_FILE, status)
                self.assertIn("?? " + lineage.UNTRACKED_FILE, status)
                run("stage", [git, "add", "--all"])
                tree = run("tree", [git, "write-tree"]).decode().strip()
                for name, expected in lineage.FINAL_TREE.items():
                    blob = run("blob", [git, "cat-file", "blob", tree + ":" + name])
                    self.assertEqual(blob, expected, name)
                self.assertEqual(processes.pending(), [])
            finally:
                owner.rollback()


class LineageStageRecordTests(unittest.TestCase):
    def _context(self, path):
        context = mock.Mock(spec=WorkspaceContext)
        context.source_baseline = {"head": BASE_OID}
        context.state = path / "state"
        context.brief_success = path / "brief-success.md"
        return context

    def _parent(self, path):
        workspace = {"id": PARENT_ID, "path": str(path), "base_oid": BASE_OID,
                     "base_ref": "refs/delegate/" + ROOT_ID + "/workspaces/" + PARENT_ID + "/base"}
        return SuccessfulTaskEvidence(
            task=_task_fixture(PARENT_ID),
            status={}, workspace={"workspace": workspace}, capture={}, outcome={}, closure={},
            session={}, capture_evidence=_capture_evidence(FIRST_OPERATION, PARENT_ID),
        )

    def test_child_admission_refusal_names_its_stage_and_keeps_the_parent_record(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            checkout = path / "parent-checkout"
            checkout.mkdir()
            record: dict[str, object] = {}
            refusal = RuntimeError("task admission failed: source overlaps managed container")
            with (
                mock.patch.object(lineage, "_run_first_success", return_value=self._parent(checkout)),
                mock.patch.object(lineage, "_dispatch", side_effect=refusal),
            ):
                with self.assertRaisesRegex(RuntimeError, "source overlaps managed container"):
                    lineage._run_lineage(self._context(path), record)
            self.assertEqual(record["completed_stages"], ["parent-captured"])
            self.assertEqual(record["current_stage"], "child-first-turn")
            self.assertEqual(object_at(record, "parent", "checkout")["workspace_id"], PARENT_ID)

    def test_parent_pinned_to_another_base_stops_before_any_child_dispatch(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            context = self._context(path)
            context.source_baseline = {"head": "9" * 40}
            with (
                mock.patch.object(lineage, "_run_first_success", return_value=self._parent(path)),
                mock.patch.object(lineage, "_dispatch") as dispatch,
            ):
                with self.assertRaisesRegex(RuntimeError, "different base commit"):
                    lineage._run_lineage(context, {})
            dispatch.assert_not_called()


    def test_parent_turn_refusal_stops_before_any_child_dispatch(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary)
            context = self._context(path)
            record = {}
            with (
                mock.patch.object(lineage, "_run_first_success",
                                  side_effect=RuntimeError("fresh task turn was not observed")),
                mock.patch.object(lineage, "_dispatch") as dispatch,
            ):
                with self.assertRaisesRegex(RuntimeError, "not observed"):
                    lineage._run_lineage(context, record)
            self.assertEqual(record["completed_stages"], [])
            self.assertEqual(record["current_stage"], "parent-captured")
            dispatch.assert_not_called()


class FreshTurnConsumerTests(unittest.TestCase):
    def test_parent_fresh_turn_gate_precedes_reuse_release_and_forget(self):
        context = mock.Mock(spec=WorkspaceContext)
        context.state, context.brief_success = Path("/state"), Path("/brief")
        task = {"task_id": CHILD_ID, "root_id": ROOT_ID, "workspace_id": CHILD_ID}
        capture = {"status": "captured"}
        evidence = {"snapshot_ref": "ref", "turn": {"status": "observed"}}
        for error in (None, RuntimeError("fresh turn baseline mismatch")):
            verify = mock.Mock(return_value=evidence, side_effect=error)
            roundtrip, release, forget = mock.Mock(), mock.Mock(), mock.Mock()
            with (
                self.subTest(error=error),
                mock.patch.multiple(
                    lifecycle, _dispatch=mock.Mock(return_value=task),
                    _adopt_workspace_container=mock.Mock(), _await_task=mock.Mock(return_value={}),
                    _collect_task=mock.Mock(), _wait_capture=mock.Mock(return_value=({}, capture)),
                    read_json=mock.Mock(return_value={"verdict": "committed"}),
                    _prove_lifetimes_closed=mock.Mock(return_value={"closed": True}),
                    _provider_session=mock.Mock(return_value={}), _capture_roundtrip=roundtrip,
                    verify_task_turn=verify, _release_and_forget=release, _forget_workspace=forget,
                ),
            ):
                if error is None:
                    parent = lifecycle._run_first_success(context)
                    self.assertIs(parent.capture_evidence, evidence)
                    roundtrip.assert_called_once_with(context, capture)
                else:
                    with self.assertRaisesRegex(RuntimeError, "fresh turn baseline mismatch") as raised:
                        lifecycle._run_success(context)
                    self.assertIs(raised.exception, error)
                    roundtrip.assert_not_called()
            verify.assert_called_once_with(context, capture, task_id=CHILD_ID,
                                           expected_files=frozenset({lineage.FIXTURE_FILE}))
            release.assert_not_called()
            forget.assert_not_called()

    def test_settlement_rejects_missing_or_unavailable_turn_before_patch_roundtrip(self):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary)
            context = mock.Mock(spec=WorkspaceContext)
            context.state = state
            task = _task_fixture(CHILD_ID)
            directory = state / "tasks" / CHILD_ID
            directory.mkdir(parents=True)
            (directory / "outcome.json").write_text(json.dumps({"verdict": "committed"}))
            capture = _write_capture(state, FIRST_OPERATION, CHILD_ID, "6" * 40)
            for turn in (None, {"status": "unavailable", "reason_code": "baseline_unavailable"}):
                object_at(capture, "candidate")["turn"] = turn
                with (
                    self.subTest(turn=turn),
                    mock.patch.object(lineage, "_await_task", return_value={}),
                    mock.patch.object(lineage, "_wait_capture", return_value=({}, capture)),
                    mock.patch.object(lineage, "_prove_lifetimes_closed", return_value={"closed": True}),
                    mock.patch.object(lineage, "_provider_session", return_value={}),
                    mock.patch.object(lineage, "_capture_roundtrip") as roundtrip,
                ):
                    with self.assertRaisesRegex(RuntimeError, "not observed"):
                        lineage._settle_turn(context, task, label="child",
                                             expected_files=frozenset({lineage.FIXTURE_FILE}))
                    roundtrip.assert_not_called()

    def test_first_child_requires_the_fixture_edit(self):
        context = mock.Mock(spec=WorkspaceContext)
        context.brief_success = Path("/brief-success.md")
        context.container_token.directory = Path("/c")
        task = _task_fixture(CHILD_ID)
        parent = lineage.CheckoutFacts(PARENT_ID, "/c/parent", BASE_OID, "refs/delegate/parent/base")
        workspace: dict[str, object] = {"workspace": {"id": CHILD_ID, "path": "/c/child", "base_oid": BASE_OID,
                                    "base_ref": "refs/delegate/child/base"}}
        first = lineage.TurnEvidence(task=task, workspace=workspace, capture=_capture_evidence(FIRST_OPERATION), session={}, turn={})
        with (
            mock.patch.object(lineage, "_dispatch", return_value=task),
            mock.patch.object(lineage, "_adopt_workspace_container"),
            mock.patch.object(lineage, "_settle_turn", return_value=first) as settle,
            mock.patch.object(lineage, "_verify_ref", return_value=0),
        ):
            lineage._run_child_first_turn(context, {}, parent)
        settle.assert_called_once_with(context, task, label="lineage-child-first-capture",
                                       expected_files=frozenset({lineage.FIXTURE_FILE}))

    def test_second_child_requires_reuse_and_excludes_the_previous_fixture_edit(self):
        context = mock.Mock(spec=WorkspaceContext)
        context.waiter_directory = Path("/fixture-waiters")
        first = lineage.TurnEvidence(task=_task_fixture(CHILD_ID), workspace={}, capture=_capture_evidence(FIRST_OPERATION),
                                     session={}, turn={})
        task = _task_fixture(CHILD_ID)
        second = lineage.TurnEvidence(task=task, workspace={}, session={}, turn={},
                                      capture=_capture_evidence(SECOND_OPERATION))
        child = lineage.CheckoutFacts(CHILD_ID, "/c/child", BASE_OID, "refs/delegate/child/base")
        record: dict[str, object] = {"completed_stages": []}
        with (
            mock.patch.object(lineage, "_start_held_turn", return_value=task),
            mock.patch.object(lineage, "_observe_task", return_value={}),
            mock.patch.object(lineage, "_release_parent", return_value={}),
            mock.patch.object(lineage, "_settle_turn", return_value=second) as settle,
            mock.patch.object(lineage.private_cli_leases, "require_empty_fixture_waiters"),
        ):
            lineage._run_second_turn(context, record, mock.Mock(), child, first, child)
        settle.assert_called_once_with(
            context, task, label="lineage-child-second-capture",
            expected_files=frozenset({lineage.REUSED_FILE}),
            forbidden_files=frozenset({lineage.FIXTURE_FILE}))


class LineageApplicationTests(unittest.TestCase):
    def _arguments(self, output, *extra):
        return ["--source-root", str(application.REPOSITORY), "--output", str(output), *extra]

    def test_lineage_is_a_scenario_without_capacity_options(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "receipt.json"
            arguments = application._parse_args(self._arguments(output, "--scenario", "lineage"))
            self.assertEqual(arguments.scenario, "lineage")
            with self.assertRaises(SystemExit), mock.patch("sys.stderr"):
                application._parse_args(self._arguments(
                    output, "--scenario", "lineage", "--parallel-tasks", "3"))

    def test_failed_lineage_run_retains_the_completed_stages_in_the_receipt(self):
        context = mock.Mock(spec=WorkspaceContext)
        context.state = Path("/missing-proof-root/state")
        context.source_baseline = {}
        context.native_proofs = []

        def partial(_context, record):
            record.update({"completed_stages": ["parent-captured"], "current_stage": "busy-window"})
            raise RuntimeError("busy window failed")

        with tempfile.TemporaryDirectory() as temporary:
            finalizer = ReceiptFinalizer(Path(temporary) / "receipt.json", [], {})
            with (
                mock.patch.object(application, "build_source_manifest", return_value={}),
                mock.patch.object(application, "_run_lineage", side_effect=partial),
                mock.patch.object(application, "_assert_source_untouched") as untouched,
            ):
                with self.assertRaisesRegex(RuntimeError, "busy window failed"):
                    application._run(mock.Mock(scenario="lineage"), finalizer, context)
            untouched.assert_not_called()
            finalizer.finalize("failed", "busy window failed", {"authorized": False})
            details = json.loads((Path(temporary) / "receipt.json").read_text())["details"]
            self.assertEqual(details["scenario"], "lineage")
            self.assertEqual(object_at(details, "lineage")["current_stage"], "busy-window")
            self.assertEqual(object_at(details, "lineage")["completed_stages"], ["parent-captured"])

    def test_passed_lineage_run_proves_the_original_source_untouched(self):
        context = mock.Mock(spec=WorkspaceContext)
        context.state = Path("/missing-proof-root/state")
        context.source_baseline = {}
        context.native_proofs = []
        context.task_ids, context.workspace_ids, context.command_count = [], [], 0
        finalizer = ReceiptFinalizer(Path("/unused"), [], {})
        with (
            mock.patch.object(application, "build_source_manifest", return_value={}),
            mock.patch.object(application, "_run_lineage"),
            mock.patch.object(application, "_assert_source_untouched",
                              return_value={"status_empty": True}) as untouched,
        ):
            verdict, _reason = application._run(mock.Mock(scenario="lineage"), finalizer, context)
        self.assertEqual(verdict, "passed")
        untouched.assert_called_once_with(context)
        self.assertEqual(finalizer.details["source_final"], {"status_empty": True})


if __name__ == "__main__":
    unittest.main()
