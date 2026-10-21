"""Oracle, ordering and cleanup-authority tests for the checkpoint acceptance scenario."""

from __future__ import annotations

import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import scripts.delegate_acceptance.private_cli.supervisor as private_cli_supervisor
import scripts.delegate_acceptance.workspaces.scenarios.application as application
import scripts.delegate_acceptance.workspaces.scenarios.checkpoints as checkpoints
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.workspaces.scenarios import checkpoint_policy as policy
from scripts.delegate_acceptance.workspaces.scenarios.context import WorkspaceContext
from scripts.delegate_acceptance.workspaces.scenarios.evidence import ReceiptFinalizer
from scripts.delegate_acceptance.workspaces.scenarios.lineage import CheckoutFacts
from scripts.delegate_acceptance.workspaces.scenarios.source import _git_executable

ROOT_ID = "e" * 32
CHECKPOINT = "c" * 32
REMOVAL = "d" * 32
NEWER = "f" * 32
WORKSPACE = "a" * 32
COMMIT = "1" * 40
BASE = "2" * 40
REF = "refs/delegate/" + ROOT_ID + "/checkpoints/" + CHECKPOINT
# The modern control fixture's explicit finite bound for observing launched children closed.
OBSERVER_DRAIN_SECONDS = 40


def _response(block=None, **extra):
    response = {"schema_version": 1, "command": "checkpoint", "root_id": ROOT_ID,
                "admission": "unknown", "liveness": "undetermined", "publication": "unknown"}
    if block is not None:
        response["checkpoint"] = block
    response.update(extra)
    return response


def _current(state="checkpointed", usable=True, **extra):
    current = {"id": CHECKPOINT, "selection": "ordinary", "cwd": "/source", "state": state,
               "snapshot_ref": REF, "commit_oid": COMMIT, "usable": usable}
    current.update(extra)
    return current


class CreationAndRemovalAcknowledgementTests(unittest.TestCase):
    def test_refused_creation_acknowledges_nothing(self):
        refusal = _response({"reason_code": policy.NOT_AUTHORIZED, "next_action": "retry_create"})
        self.assertEqual(policy.classify_creation(3, refusal, ROOT_ID), policy.Acknowledgement(accepted=False))
        with self.assertRaises(AcceptanceFailure):
            policy.classify_creation(3, _response({"id": CHECKPOINT, "reason_code": policy.NOT_AUTHORIZED,
                                                   "next_action": "retry_create"}), ROOT_ID)

    def test_accepted_creation_is_pending_or_settled_but_never_a_failed_receipt(self):
        pending = policy.classify_creation(3, _response({"id": CHECKPOINT, "next_action": policy.WATCH}), ROOT_ID)
        self.assertEqual((pending.accepted, pending.checkpoint_id, pending.settled), (True, CHECKPOINT, False))
        settled = policy.classify_creation(0, _response({"id": CHECKPOINT, "current": _current()}), ROOT_ID)
        self.assertTrue(settled.settled)
        failed = _response({"id": CHECKPOINT, "next_action": policy.STOP_AND_REPORT}, error="joined fault")
        with self.assertRaisesRegex(AcceptanceFailure, "joined fault"):
            policy.classify_creation(1, failed, ROOT_ID)
        with self.assertRaises(AcceptanceFailure):
            policy.classify_creation(0, _response({"id": CHECKPOINT, "current": _current("unavailable", False)}),
                                     ROOT_ID)

    def test_checkpoint_responses_never_borrow_task_or_workspace_authority(self):
        for key in ("task_id", "outcome", "workspace"):
            with self.subTest(key=key), self.assertRaisesRegex(AcceptanceFailure, "foreign authority"):
                policy.classify_creation(3, _response({"id": CHECKPOINT, "next_action": policy.WATCH},
                                                      **{key: "x"}), ROOT_ID)
        with self.assertRaisesRegex(AcceptanceFailure, "different state root"):
            policy.classify_creation(3, _response({"id": CHECKPOINT, "next_action": policy.WATCH}), "b" * 32)

    def test_removal_names_its_exact_attempt_or_nothing(self):
        refusal = _response({"reason_code": policy.NOT_AUTHORIZED, "next_action": "retry_remove"})
        self.assertFalse(policy.classify_removal(3, refusal, ROOT_ID, CHECKPOINT).accepted)
        accepted = policy.classify_removal(
            3, _response({"id": CHECKPOINT, "operation_id": REMOVAL, "next_action": policy.WATCH}),
            ROOT_ID, CHECKPOINT)
        self.assertEqual((accepted.operation_id, accepted.settled), (REMOVAL, False))
        operation = {"operation_id": REMOVAL, "attempts": 1, "state": "removed"}
        settled = policy.classify_removal(
            0, _response({"id": CHECKPOINT, "operation_id": REMOVAL, "operation": operation}), ROOT_ID, CHECKPOINT)
        self.assertTrue(settled.settled)
        for block in ({"id": CHECKPOINT, "operation_id": CHECKPOINT, "next_action": policy.WATCH},
                      {"id": NEWER, "operation_id": REMOVAL, "next_action": policy.WATCH}):
            with self.subTest(block=block), self.assertRaises(AcceptanceFailure):
                policy.classify_removal(3, _response(block), ROOT_ID, CHECKPOINT)


class StatusObservationTests(unittest.TestCase):
    def test_pending_status_returns_nothing_and_never_describes_settled_state(self):
        self.assertIsNone(policy.classify_status(3, _response({"next_action": policy.WATCH}), ROOT_ID, CHECKPOINT))
        pending = _response({"id": CHECKPOINT, "current": _current("pending", False), "next_action": policy.WATCH})
        self.assertIsNone(policy.classify_status(3, pending, ROOT_ID, CHECKPOINT))
        with self.assertRaises(AcceptanceFailure):
            policy.classify_status(3, _response({"id": CHECKPOINT, "current": _current(),
                                                 "next_action": policy.WATCH}), ROOT_ID, CHECKPOINT)

    def test_successful_observation_of_unavailable_history_is_not_usability(self):
        history = _current("unavailable", False, reason_code="checkpoint_failed")
        observed = policy.classify_status(
            0, _response({"id": CHECKPOINT, "current": history, "next_action": policy.STOP_AND_REPORT}),
            ROOT_ID, CHECKPOINT)
        self.assertEqual(observed["state"], "unavailable")
        with self.assertRaisesRegex(AcceptanceFailure, "not a usable"):
            policy.qualified_checkpoint(observed, root_id=ROOT_ID, selection="ordinary", cwd="/source")
        with self.assertRaisesRegex(AcceptanceFailure, "guidance"):
            policy.classify_status(0, _response({"id": CHECKPOINT, "current": history}), ROOT_ID, CHECKPOINT)

    def test_usability_must_match_the_described_history(self):
        removal = {"operation_id": REMOVAL, "attempts": 1, "state": "removed"}
        for current in (_current(usable=False), _current(removal=removal)):
            with self.subTest(current=current), self.assertRaisesRegex(AcceptanceFailure, "usability"):
                policy.classify_status(0, _response({"id": CHECKPOINT, "current": current}), ROOT_ID, CHECKPOINT)

    def test_selected_historical_failure_is_not_replaced_by_a_later_successful_attempt(self):
        # Valid history: every predecessor of the head holds an unavailable receipt.
        latest = {"operation_id": NEWER, "attempts": 2, "state": "removed"}
        selected = {"operation_id": REMOVAL, "attempts": 2, "state": "unavailable",
                    "reason_code": "checkpoint_removal_failed"}
        history = _current(usable=False, removal=latest)
        failed = _response({"id": CHECKPOINT, "operation_id": REMOVAL, "operation": selected,
                            "current": history, "next_action": policy.STOP_AND_REPORT},
                           error="checkpoint removal attempt is unavailable")
        with self.assertRaisesRegex(AcceptanceFailure, "exit 1 .*unavailable"):
            policy.classify_removal_status(1, failed, ROOT_ID, checkpoint_id=CHECKPOINT, operation_id=REMOVAL)
        forged = _response({"id": CHECKPOINT, "operation_id": REMOVAL, "operation": selected, "current": history})
        with self.assertRaisesRegex(AcceptanceFailure, "did not settle successfully"):
            policy.classify_removal_status(0, forged, ROOT_ID, checkpoint_id=CHECKPOINT, operation_id=REMOVAL)
        borrowed = _response({"id": CHECKPOINT, "operation_id": NEWER, "operation": latest, "current": history})
        with self.assertRaisesRegex(AcceptanceFailure, "another attempt"):
            policy.classify_removal_status(0, borrowed, ROOT_ID, checkpoint_id=CHECKPOINT, operation_id=REMOVAL)

    def test_selected_successful_latest_attempt_settles(self):
        latest = {"operation_id": NEWER, "attempts": 2, "state": "removed"}
        settled = _response({"id": CHECKPOINT, "operation_id": NEWER, "operation": latest,
                             "current": _current(usable=False, removal=latest)})
        self.assertEqual(policy.classify_removal_status(
            0, settled, ROOT_ID, checkpoint_id=CHECKPOINT, operation_id=NEWER), latest)

    def test_selected_unavailable_or_foreign_attempt_fails(self):
        unavailable = {"operation_id": REMOVAL, "attempts": 1, "state": "unavailable"}
        failed = _response({"id": CHECKPOINT, "operation_id": REMOVAL, "operation": unavailable,
                            "next_action": policy.STOP_AND_REPORT}, error="checkpoint removal attempt is unavailable")
        with self.assertRaisesRegex(AcceptanceFailure, "unavailable"):
            policy.classify_removal_status(1, failed, ROOT_ID, checkpoint_id=CHECKPOINT, operation_id=REMOVAL)
        foreign = _response({"id": CHECKPOINT, "operation_id": NEWER,
                             "operation": {"operation_id": NEWER, "attempts": 2, "state": "removed"}})
        with self.assertRaisesRegex(AcceptanceFailure, "another attempt"):
            policy.classify_removal_status(0, foreign, ROOT_ID, checkpoint_id=CHECKPOINT, operation_id=REMOVAL)

    def test_removed_history_requires_the_exact_single_attempt(self):
        facts = policy.qualified_checkpoint(_current(), root_id=ROOT_ID, selection="ordinary", cwd="/source")
        removal = {"operation_id": REMOVAL, "attempts": 1, "state": "removed"}
        policy.removed_history(_current(usable=False, removal=removal), facts, REMOVAL)
        with self.assertRaises(AcceptanceFailure):
            policy.removed_history(_current(usable=False, removal={**removal, "attempts": 2}), facts, REMOVAL)

    def test_qualified_checkpoint_binds_target_and_owned_ref(self):
        for current, target in ((_current(snapshot_ref=REF + "x"), {"cwd": "/source"}),
                                (_current(), {"cwd": "/other"}),
                                (_current(), {"workspace_id": WORKSPACE, "cwd": "/source"})):
            with self.subTest(target=target), self.assertRaises(AcceptanceFailure):
                policy.qualified_checkpoint(current, root_id=ROOT_ID, selection="ordinary", **target)


class ListAndRefOracleTests(unittest.TestCase):
    def _page(self, entries, cursor=None):
        extra = {"checkpoints": entries}
        if cursor is not None:
            extra["checkpoint_cursor"] = cursor
        return _response(**extra)

    def test_pages_are_ascending_and_cursor_names_the_last_full_entry(self):
        first, second = _current(id="1" * 32), _current(id="2" * 32)
        self.assertEqual(policy.list_page(self._page([first], "1" * 32), ROOT_ID, limit=1, after=""),
                         ([first], "1" * 32))
        self.assertEqual(policy.list_page(self._page([second]), ROOT_ID, limit=1, after="1" * 32), ([second], ""))
        self.assertEqual(policy.list_page(self._page([]), ROOT_ID, limit=1, after=""), ([], ""))
        for response, after in ((self._page([first], "2" * 32), ""), (self._page([first]), "1" * 32),
                                (self._page([first, second]), ""),
                                (_response({"id": CHECKPOINT}, checkpoints=[]), "")):
            with self.subTest(response=response), self.assertRaises(AcceptanceFailure):
                policy.list_page(response, ROOT_ID, limit=1, after=after)

    def _observe(self, **overrides):
        values = {"reference": REF, "inventory": b"", "inventory_stderr": b"", "symbolic_exit": 1,
                  "symbolic_stdout": b"", "resolved_exit": 1, "resolved_stdout": b""}
        values.update(overrides)
        return policy.ref_observation(**values)

    def test_ref_presence_and_proven_absence(self):
        self.assertIsNone(self._observe())
        present = (REF + " " + COMMIT + "\n").encode()
        self.assertEqual(self._observe(inventory=present, resolved_exit=0,
                                       resolved_stdout=(COMMIT + "\n").encode()), COMMIT)

    def test_inventory_absence_alone_is_not_proof(self):
        for overrides in ({"symbolic_exit": 0, "symbolic_stdout": b"refs/heads/none\n"},
                          {"resolved_exit": 0, "resolved_stdout": (COMMIT + "\n").encode()},
                          {"inventory_stderr": b"warning: ignoring broken ref\n"}):
            with self.subTest(overrides=overrides), self.assertRaises(AcceptanceFailure):
                self._observe(**overrides)

    def test_unchanged_checkout_detects_a_rewritten_index(self):
        before = {"head": BASE, "index": {"bytes": 1, "sha256": "0" * 64, "device": 1, "inode": 2}}
        policy.unchanged_checkout(before, json.loads(json.dumps(before)), "creation")
        with self.assertRaisesRegex(AcceptanceFailure, "creation changed its index"):
            policy.unchanged_checkout(before, {**before, "index": {**before["index"], "inode": 3}}, "creation")

    def test_closure_facts_project_native_scope(self):
        closure = {"mode": "terminated", "request_sha256": "9" * 64,
                   "managed_lifetime": {"intent": {"scope_kind": "cgroup-v2", "assurance": "exact"}}}
        self.assertEqual(policy.closure_facts(closure), {"mode": "terminated", "request_sha256": "9" * 64,
                                                         "scope_kind": "cgroup-v2", "assurance": "exact"})
        self.assertEqual(policy.closure_facts({"mode": "unlaunched"}), {"mode": "unlaunched", "request_sha256": None})


def settle_observer_resources(owner: DirectoryOwner, processes: Processes | None, root: Path) -> None:
    """Remove the owned root only after every launched process is observed closed naturally.

    Unresolved, non-natural or unobservable closure retains the whole root and
    its captures and consumes only the directory pins; no child is signalled.
    """
    if processes is not None:
        try:
            pending = processes.drain(OBSERVER_DRAIN_SECONDS)
            closed = not pending and processes.finished_naturally()
        except BaseException as error:
            _retain_observer_root(owner, root, "observer process closure is unobservable", error)
        if not closed:
            _retain_observer_root(owner, root, "observer processes did not close naturally", None)
    owner.rollback()


def _retain_observer_root(owner: DirectoryOwner, root: Path, reason: str, cause: BaseException | None) -> None:
    retained = RuntimeError(reason + "; retained: " + str(root))
    try:
        owner.release()
    except (OSError, RuntimeError) as release_error:
        retained.add_note("directory pin release failed: " + str(release_error))
    raise retained from cause


class RealGitObserverTests(unittest.TestCase):
    """The ref and checkout observers run real Git through the canonical process owner."""

    def setUp(self):
        self.owner = DirectoryOwner()
        self.processes = None
        parent = Path(tempfile.gettempdir()).resolve()
        root = Path(tempfile.mkdtemp(prefix="checkpoint-observer-", dir=parent)).resolve()
        self.root = self.owner.adopt_created("checkpoint observer root", root, parent)
        self.addCleanup(self._settle)
        self.repository = self.owner.create("checkpoint observer repository", root / "repository", root).directory
        captures = self.owner.create("checkpoint observer captures", root / "captures", root)
        environment = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        environment.update({"GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"})
        self.processes = Processes(captures.directory, capture_root=captures, environment=environment,
                                   environment_keys_only=True)
        self.context = mock.Mock(spec=WorkspaceContext)
        self.context.processes = self.processes
        self._initialize_repository()

    def _settle(self):
        settle_observer_resources(self.owner, self.processes, self.root.directory)

    def _initialize_repository(self):
        self.git(["init", "--quiet"])
        (self.repository / "tracked.txt").write_bytes(b"base\n")
        self.git(["add", "--", "tracked.txt"])
        self.git(["-c", "user.name=T", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-m", "base"])
        self.head = self.git(["rev-parse", "HEAD"]).decode().strip()

    def git(self, arguments):
        return self.processes.run("git", [_git_executable(), "-C", str(self.repository), *arguments],
                                  self.repository, stdin=b"").output()

    def test_owned_ref_proves_presence_absence_and_refuses_a_dangling_symref(self):
        self.assertIsNone(checkpoints._owned_ref(self.context, self.repository, REF, label="absent"))
        self.git(["update-ref", REF, self.head])
        self.assertEqual(checkpoints._owned_ref(self.context, self.repository, REF, label="present"), self.head)
        self.git(["update-ref", "-d", REF])
        self.git(["symbolic-ref", REF, "refs/heads/missing"])
        with self.assertRaisesRegex(AcceptanceFailure, "symbolic"):
            checkpoints._owned_ref(self.context, self.repository, REF, label="dangling")
        self.assertEqual(self.processes.pending(), [])

    def test_checkout_observer_never_writes_the_real_index(self):
        (self.repository / "tracked.txt").write_bytes(b"dirty\n")
        before = checkpoints._observe_checkout(self.context, self.repository, label="before")
        after = checkpoints._observe_checkout(self.context, self.repository, label="after")
        policy.unchanged_checkout(before, after, "observation")
        self.assertEqual(before["head"], self.head)
        index = self.repository / ".git" / "index"
        replacement = index.with_name("index.replacement")
        replacement.write_bytes(index.read_bytes())
        replacement.replace(index)
        with self.assertRaisesRegex(AcceptanceFailure, "changed its index"):
            policy.unchanged_checkout(
                before, checkpoints._observe_checkout(self.context, self.repository, label="rewritten"), "creation")



class ObserverCleanupControlTests(unittest.TestCase):
    """Only proven natural closure removes the observer root; anything else retains it."""

    def _owned_root(self, parent: Path) -> tuple[DirectoryOwner, Path, list]:
        owner = DirectoryOwner()
        root = owner.create("control root", parent / "root", parent)
        captures = owner.create("control captures", root.directory / "captures", root.directory)
        return owner, root.directory, [root, captures]

    def _pending_processes(self, *, drain=(), natural=True, error=None):
        processes = mock.Mock(spec=Processes)
        processes.drain.return_value = list(drain)
        processes.drain.side_effect = error
        processes.finished_naturally.return_value = natural
        return processes

    def test_natural_closure_including_a_nonzero_exit_removes_the_root(self):
        owner = DirectoryOwner()
        parent = Path(tempfile.gettempdir()).resolve()
        root = owner.adopt_created("control root", Path(tempfile.mkdtemp(prefix="checkpoint-control-",
                                                                         dir=parent)).resolve(), parent)
        launched: list[Processes] = []
        settled: list[bool] = []

        def settle_unless_proven():
            if not settled:
                settle_observer_resources(owner, launched[0] if launched else None, root.directory)

        self.addCleanup(settle_unless_proven)
        captures = owner.create("control captures", root.directory / "captures", root.directory)
        launched.append(Processes(captures.directory, capture_root=captures, environment={"PATH": "/usr/bin:/bin"}))
        launched[0].run("true", ["/bin/sh", "-c", "exit 0"], root.directory)
        launched[0].run("false", ["/bin/sh", "-c", "exit 1"], root.directory, expected=1)
        settle_observer_resources(owner, launched[0], root.directory)
        settled.append(True)
        self.assertFalse(os.path.lexists(root.directory))

    def test_unresolved_unnatural_or_unobservable_closure_retains_the_root(self):
        cases = (
            ("did not close naturally", self._pending_processes(
                drain=[{"pid": 4242, "argv": ["git"], "directory": "000-git"}])),
            ("did not close naturally", self._pending_processes(natural=False)),
            ("is unobservable", self._pending_processes(error=RuntimeError("completed receipt mismatch"))),
        )
        for reason, processes in cases:
            with self.subTest(reason=reason), tempfile.TemporaryDirectory() as temporary:
                owner, root, tokens = self._owned_root(Path(temporary).resolve())
                with self.assertRaisesRegex(RuntimeError, reason + "; retained: " + str(root)) as raised:
                    settle_observer_resources(owner, processes, root)
                processes.drain.assert_called_once_with(OBSERVER_DRAIN_SECONDS)
                self.assertTrue((root / "captures").is_dir())
                self.assertTrue(all(token.consumed for token in tokens))
                self.assertIs(raised.exception.__cause__, processes.drain.side_effect)

    def _run_fixture(self, initialize) -> tuple[unittest.TestResult, list[Path]]:
        roots: list[Path] = []

        class Fixture(RealGitObserverTests):
            def _initialize_repository(self):
                roots.append(self.root.directory)
                initialize(self)

        result = unittest.TestResult()
        Fixture("test_owned_ref_proves_presence_absence_and_refuses_a_dangling_symref").run(result)
        return result, roots

    def test_setup_failure_before_any_launch_removes_the_owned_root(self):
        def fail(fixture):
            self.assertEqual(fixture.processes.entries, [])
            raise RuntimeError("setup failed before launch")

        result, roots = self._run_fixture(fail)
        self.assertEqual([str(error[1]).splitlines()[-1] for error in result.errors],
                         ["RuntimeError: setup failed before launch"])
        self.assertFalse(os.path.lexists(roots[0]))

    def test_setup_failure_and_retained_cleanup_are_both_reported(self):
        def fail(fixture):
            fixture.processes = self._pending_processes(natural=False)
            raise RuntimeError("setup failed after launch")

        with tempfile.TemporaryDirectory() as temporary, \
                mock.patch.object(tempfile, "gettempdir", return_value=temporary):
            result, roots = self._run_fixture(fail)
            self.assertTrue((roots[0] / "captures").is_dir())
        messages = [str(error[1]) for error in result.errors]
        self.assertEqual(len(messages), 2)
        self.assertIn("setup failed after launch", messages[0])
        self.assertIn("did not close naturally; retained: " + str(roots[0]), messages[1])

class ScenarioOrderingTests(unittest.TestCase):
    def setUp(self):
        self.context = mock.Mock(spec=WorkspaceContext)
        self.record = {"commands": []}
        patcher = mock.patch.object(checkpoints, "_root_id", return_value=ROOT_ID)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_only_a_refusal_is_retried_and_an_accepted_creation_is_not_repeated(self):
        refusal = (3, _response({"reason_code": policy.NOT_AUTHORIZED, "next_action": "retry_create"}))
        accepted = (3, _response({"id": CHECKPOINT, "next_action": policy.WATCH}))
        with (mock.patch.object(checkpoints, "_checkpoint_command", side_effect=[refusal, accepted]) as command,
              mock.patch.object(checkpoints.time, "sleep")):
            acknowledgement = checkpoints._accepted_creation(
                self.context, self.record, ["--workspace", WORKSPACE], label="create")
        self.assertEqual((acknowledgement.checkpoint_id, command.call_count), (CHECKPOINT, 2))
        with mock.patch.object(checkpoints, "_checkpoint_command",
                               return_value=(1, _response({"id": CHECKPOINT, "next_action": policy.STOP_AND_REPORT}))
                               ) as command, self.assertRaises(AcceptanceFailure):
            checkpoints._accepted_creation(self.context, self.record, ["--cwd", "/source"], label="create")
        command.assert_called_once()

    def test_removal_waits_on_exactly_its_acknowledged_attempt(self):
        pending = (3, _response({"id": CHECKPOINT, "operation_id": REMOVAL, "next_action": policy.WATCH}))
        removed = {"operation_id": REMOVAL, "attempts": 1, "state": "removed"}
        settled = (0, _response({"id": CHECKPOINT, "operation_id": REMOVAL, "operation": removed}))
        acknowledgement = policy.Acknowledgement(accepted=True, checkpoint_id=CHECKPOINT, operation_id=REMOVAL)
        with mock.patch.object(checkpoints, "_checkpoint_command", side_effect=[pending, settled]) as command:
            self.assertEqual(checkpoints._await_removal(self.context, self.record, acknowledgement, label="r"),
                             removed)
        for call in command.call_args_list:
            arguments = call.args[2]
            self.assertEqual(arguments[:4], ["status", CHECKPOINT, "--operation", REMOVAL])
            self.assertNotIn("remove", arguments)

    def test_busy_managed_creation_must_be_refused_and_is_never_retargeted(self):
        parent = mock.Mock(workspace={"workspace": {"id": WORKSPACE, "path": "/c/parent", "base_oid": BASE,
                                                    "base_ref": "refs/delegate/" + ROOT_ID + "/base"}})
        accepted = policy.Acknowledgement(accepted=True, checkpoint_id=CHECKPOINT)
        with (mock.patch.object(checkpoints, "_run_first_success", return_value=parent),
              mock.patch.object(checkpoints, "_start_held_turn", return_value={"task_id": NEWER}),
              mock.patch.object(checkpoints, "_create_once", return_value=accepted) as create,
              mock.patch.object(checkpoints, "_settle_turn") as settle):
            with self.assertRaisesRegex(RuntimeError, "acknowledged a checkpoint creation"):
                checkpoints._refuse_busy_managed(self.context, self.record, known=[])
        create.assert_called_once_with(self.context, self.record, ["--workspace", WORKSPACE],
                                       label="checkpoint-create-busy")
        settle.assert_not_called()
        self.context.release_gates.assert_not_called()

    def test_checkpoint_fork_requires_a_moved_base_unlike_lineage(self):
        facts = policy.CheckpointFacts(CHECKPOINT, COMMIT, REF)
        parent = CheckoutFacts(WORKSPACE, "/c/parent", BASE, "refs/delegate/r/a/base")
        child = CheckoutFacts(NEWER, "/c/child", COMMIT, "refs/delegate/r/f/base")
        checkpoints.require_checkpoint_fork(child, facts, origin_base=BASE, container=Path("/c"), parent=parent)
        for bad, origin in ((CheckoutFacts(NEWER, "/c/child", BASE, child.base_ref), BASE),
                            (child, COMMIT),
                            (CheckoutFacts(NEWER, "/c/parent/child", COMMIT, child.base_ref), BASE),
                            (CheckoutFacts(NEWER, "/c/child", COMMIT, parent.base_ref), BASE)):
            with self.subTest(bad=bad, origin=origin), self.assertRaises(RuntimeError):
                checkpoints.require_checkpoint_fork(bad, facts, origin_base=origin, container=Path("/c"),
                                                    parent=parent)


class CheckpointApplicationTests(unittest.TestCase):
    def _context(self):
        context = mock.Mock(spec=WorkspaceContext)
        context.state = Path("/missing-proof-root/state")
        context.source_baseline = {}
        context.native_proofs = []
        context.task_ids, context.workspace_ids, context.command_count = [], [], 0
        return context

    def test_checkpoint_is_a_selectable_scenario_without_capacity_options(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "receipt.json"
            base = ["--delegate", "d", "--runner", "r", "--pueue", "p", "--pueued", "q", "--output", str(output)]
            self.assertEqual(application._parse_args([*base, "--scenario", "checkpoint"]).scenario, "checkpoint")
            with self.assertRaises(SystemExit), mock.patch("sys.stderr"):
                application._parse_args([*base, "--scenario", "checkpoint", "--parallel-tasks", "3"])

    def test_failed_stage_stays_in_the_receipt_and_skips_the_clean_source_proof(self):
        def partial(_context, record):
            record.update({"completed_stages": ["ordinary-checkpoint"], "current_stage": "ordinary-fork"})
            raise RuntimeError("fork failed")

        with tempfile.TemporaryDirectory() as temporary:
            finalizer = ReceiptFinalizer(Path(temporary) / "receipt.json", [], {})
            with (mock.patch.object(application, "build_source_manifest", return_value={}),
                  mock.patch.object(application, "_run_checkpoints", side_effect=partial),
                  mock.patch.object(application, "_assert_source_untouched") as untouched):
                with self.assertRaisesRegex(RuntimeError, "fork failed"):
                    application._run(mock.Mock(scenario="checkpoint"), finalizer, self._context())
            untouched.assert_not_called()
            finalizer.finalize("failed", "fork failed", {"authorized": False})
            receipt = json.loads((Path(temporary) / "receipt.json").read_text())
        self.assertEqual((receipt["claim_class"], receipt["details"]["scenario"]),
                         ("native-fixture-only", "checkpoint"))
        self.assertEqual(receipt["details"]["checkpoint"]["current_stage"], "ordinary-fork")

    def test_passed_run_still_proves_the_clean_source_untouched(self):
        finalizer = ReceiptFinalizer(Path("/unused"), [], {})
        context = self._context()
        with (mock.patch.object(application, "build_source_manifest", return_value={}),
              mock.patch.object(application, "_run_checkpoints"),
              mock.patch.object(application, "_assert_source_untouched",
                                return_value={"status_empty": True}) as untouched):
            self.assertEqual(application._run(mock.Mock(scenario="checkpoint"), finalizer, context)[0], "passed")
        untouched.assert_called_once_with(context)


class CheckpointOnlyRootClosureTests(unittest.TestCase):
    def test_cleanup_authority_scans_operation_journals_without_any_task(self):
        with tempfile.TemporaryDirectory() as temporary:
            state = Path(temporary)
            (state / "root.json").write_text(json.dumps({
                "schema_version": 1, "root_id": ROOT_ID, "created_at": "2026-10-21T00:00:00Z"}))
            (state / "workspace-operations" / CHECKPOINT).mkdir(parents=True)
            with self.assertRaisesRegex(RuntimeError, "workspace operation request is missing"):
                private_cli_supervisor.await_managed_lifetime_closure(state, (), timeout=0)


if __name__ == "__main__":
    unittest.main()
