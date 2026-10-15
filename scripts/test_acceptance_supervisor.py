"""Supervisor gate failures must remain failures and expose their evidence."""
from contextlib import redirect_stderr, redirect_stdout
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import acceptance_supervisor_common as supervisor_common
import acceptance_supervisor_hermetic as hermetic
from acceptance_supervisor_hermetic import Case, HermeticSuite


class OwnedDirectoryReadTests(unittest.TestCase):
    def test_live_read_rejects_replaced_parent(self):
        with tempfile.TemporaryDirectory() as temporary:
            parent = Path(temporary) / "parent"
            parent.mkdir()
            owner = supervisor_common.DirectoryOwner()
            try:
                token = owner.create("capture root", parent / "capture", parent)
                token.assert_live()
                original = Path(temporary) / "original-parent"
                parent.rename(original)
                try:
                    parent.mkdir()
                    with self.assertRaisesRegex(RuntimeError, "parent was replaced"):
                        token.assert_live()
                finally:
                    if parent.exists():
                        parent.rename(Path(temporary) / "replacement-parent")
                    original.rename(parent)
            finally:
                owner.release()

    def test_live_read_rejects_replaced_directory(self):
        with tempfile.TemporaryDirectory() as temporary:
            parent = Path(temporary)
            owner = supervisor_common.DirectoryOwner()
            try:
                token = owner.create("capture root", parent / "capture", parent)
                token.assert_live()
                original = parent / "original-capture"
                token.directory.rename(original)
                try:
                    token.directory.mkdir()
                    with self.assertRaisesRegex(RuntimeError, "directory was replaced"):
                        token.assert_live()
                finally:
                    if token.directory.exists():
                        token.directory.rename(parent / "replacement-capture")
                    original.rename(token.directory)
            finally:
                owner.release()


class WaitUntilTests(unittest.TestCase):
    def test_default_timeout_preserves_failure_contract(self):
        with self.assertRaisesRegex(RuntimeError, "observation expired: fixture"):
            supervisor_common.wait_until(lambda: False, timeout=0, description="fixture")

    def test_custom_timeout_raises_the_supplied_exception(self):
        expired = subprocess.TimeoutExpired(["fixture"], 0)
        with self.assertRaises(subprocess.TimeoutExpired) as raised:
            supervisor_common.wait_until(lambda: False, timeout=0, timeout_error=expired)
        self.assertIs(raised.exception, expired)

    def test_custom_timeout_preserves_predicate_failure(self):
        failure = OSError("injected observation failure")

        def predicate():
            raise failure

        with self.assertRaises(OSError) as raised:
            supervisor_common.wait_until(
                predicate, timeout=0,
                timeout_error=subprocess.TimeoutExpired(["fixture"], 0),
            )
        self.assertIs(raised.exception, failure)

    def test_custom_timeout_preserves_retry_and_success_value(self):
        result = {"ready": True}
        values = iter((False, result))
        with patch.object(supervisor_common.time, "monotonic", return_value=0), \
                patch.object(supervisor_common.time, "sleep"):
            observed = supervisor_common.wait_until(
                lambda: next(values), timeout=1,
                timeout_error=subprocess.TimeoutExpired(["fixture"], 1),
            )
        self.assertIs(observed, result)


class SupervisorDiagnosticsTests(unittest.TestCase):
    def test_hermetic_case_constructor_rolls_back_allocated_base(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary) / "case"

            def allocate(**_kwargs):
                base.mkdir()
                return str(base)

            with patch("acceptance_supervisor_hermetic.tempfile.mkdtemp",
                       side_effect=allocate), \
                    patch.object(Case, "_create_layout",
                                 side_effect=OSError("injected layout failure")):
                with self.assertRaisesRegex(OSError, "layout failure"):
                    Case(SimpleNamespace(), "constructor")
            self.assertFalse(base.exists())

    def test_hermetic_case_constructor_rolls_back_when_base_resolve_fails(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary) / "case"
            canonical_base = base.parent.resolve(strict=True) / base.name

            def allocate(**_kwargs):
                base.mkdir()
                return str(base)

            original_resolve = hermetic.Path.resolve

            def fail_base_resolve(path, *args, **kwargs):
                if path == canonical_base:
                    raise OSError("injected base resolve failure")
                return original_resolve(path, *args, **kwargs)

            with patch.object(hermetic.tempfile, "mkdtemp", side_effect=allocate), \
                    patch.object(hermetic.Path, "resolve", new=fail_base_resolve):
                with self.assertRaisesRegex(OSError, "base resolve failure"):
                    Case(SimpleNamespace(), "resolve")
            self.assertFalse(base.exists())

    def test_failed_process_construction_rolls_back_its_owned_capture(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            processes = supervisor_common.Processes(root / "processes", {"PATH": "/usr/bin"})
            with patch.object(supervisor_common.subprocess, "Popen",
                              side_effect=OSError("injected spawn failure")):
                with self.assertRaisesRegex(OSError, "spawn failure"):
                    processes.start("failed", ["/bin/sh"], root)
            self.assertEqual(processes.entries, [])
            self.assertEqual(list(processes.directory.iterdir()), [])

    def test_process_constructor_collision_keeps_preexisting_capture(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            processes = supervisor_common.Processes(root / "processes", {"PATH": "/usr/bin"})
            collision = processes.directory / "001-collision"
            collision.mkdir()
            sentinel = collision / "preexisting.txt"
            sentinel.write_text("keep")

            with self.assertRaises(FileExistsError):
                processes.start("collision", ["/bin/sh"], root)

            self.assertEqual(processes.entries, [])
            self.assertEqual(sentinel.read_text(), "keep")

    def test_drain_retains_signaled_child_as_unresolved(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            processes = supervisor_common.Processes(root / "processes")
            process = processes.start(
                "signaled",
                [sys.executable, "-c", "import os, signal; os.kill(os.getpid(), signal.SIGTERM)"],
                root,
            )

            pending = processes.drain(timeout=1)

            self.assertEqual(len(pending), 1)
            self.assertEqual(pending[0]["pid"], process.pid)
            self.assertFalse(process.result["natural_wait"])
            self.assertFalse(processes.finished_naturally())

    def test_directory_owner_rolls_back_only_created_exact_paths(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            shared = root / "shared"
            shared.mkdir()
            parent = shared / "owned-parent"
            child = parent / "owned-child"
            owner = supervisor_common.DirectoryOwner()
            owner.create("parent", parent, shared)
            owner.create("child", child, parent)

            self.assertTrue(owner.rollback())
            self.assertFalse(parent.exists())
            self.assertTrue(shared.is_dir())

    def test_directory_owner_does_not_create_a_preexisting_collision(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            parent = root / "parent"
            parent.mkdir()
            collision = parent / "run"
            collision.mkdir()
            sentinel = collision / "preexisting.txt"
            sentinel.write_text("keep")
            owner = supervisor_common.DirectoryOwner()

            with self.assertRaises(FileExistsError):
                owner.create("run", collision, parent)

            self.assertTrue(owner.rollback())
            self.assertEqual(sentinel.read_text(), "keep")

    def test_directory_owner_rejects_replaced_path_during_finalization(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            parent = root / "parent"
            parent.mkdir()
            owned = parent / "run"
            owner = supervisor_common.DirectoryOwner()
            owner.create("run", owned, parent)
            transferred = owner.transfer()

            owned.rename(parent / "original")
            owned.mkdir()
            (owned / "replacement.txt").write_text("keep")

            self.assertFalse(supervisor_common.finalize_acceptance_artifacts(
                output, True, transferred, True,
            ))
            self.assertTrue((owned / "replacement.txt").is_file())
            self.assertTrue((parent / "original").is_dir())

    def test_process_start_receipt_failure_retains_spawned_process(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            cwd = root / "cwd"
            cwd.mkdir()

            class FakePopen:
                def __init__(self, *_args, **_kwargs):
                    self.pid = 701
                    self.returncode = None

                def poll(self):
                    return self.returncode

                def wait(self):
                    return self.returncode

            original_write_json = supervisor_common.write_json

            def fail_started(path, *args, **kwargs):
                if Path(path).name == "started.json":
                    raise OSError("injected started receipt failure")
                return original_write_json(path, *args, **kwargs)

            processes = supervisor_common.Processes(root / "processes", {"PATH": "/usr/bin"})
            lifecycle_owner = SimpleNamespace(process=None)
            with patch.object(supervisor_common.subprocess, "Popen", FakePopen), \
                    patch.object(supervisor_common, "write_json", side_effect=fail_started):
                with self.assertRaisesRegex(OSError, "started receipt"):
                    processes.start("caller", ["/bin/sh"], cwd,
                                    on_spawned=lambda process: setattr(lifecycle_owner, "process", process))

            self.assertEqual(len(processes.entries), 1)
            process = processes.entries[0]
            self.assertIs(lifecycle_owner.process, process)
            self.assertEqual(process.pid, 701)
            self.assertEqual(processes.drain(timeout=0), [{
                "pid": 701, "argv": ["/bin/sh"],
                "directory": str(root / "processes" / "001-caller"),
            }])

            process.process.returncode = 0
            self.assertEqual(process.poll()["exit_code"], 0)
            self.assertEqual(processes.drain(timeout=0), [])

    def test_process_ownership_transfer_failure_retains_spawned_process(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            processes = supervisor_common.Processes(root / "processes")
            with patch.object(supervisor_common.DirectoryOwner, "transfer_child_to",
                              side_effect=OSError("injected ownership transfer failure")):
                with self.assertRaisesRegex(OSError, "ownership transfer failure"):
                    processes.start(
                        "finite-child",
                        [sys.executable, "-c", "pass"],
                        root,
                    )

            self.assertEqual(len(processes.entries), 1)
            process = processes.entries[0]
            self.assertIsNotNone(process.process)
            self.assertEqual(process.wait(timeout=10, expected=0), 0)
            self.assertEqual(processes.drain(timeout=0), [])

    def test_failed_case_snapshot_retains_logs_and_symlinks(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            case = object.__new__(Case)
            case.base = root / "private-base"
            case.base.mkdir()
            case.case_id = "failed-case"
            case.suite = SimpleNamespace(output=root / "evidence")
            case.processes = SimpleNamespace(drain=lambda **_kwargs: [{"pid": 123, "termination": "unknown"}])
            (case.base / "stderr").write_bytes(b"diagnostic bytes")
            (case.base / "unavailable-link").symlink_to(root / "not-present")
            case.retain_failure(RuntimeError("injected failure"))
            destination = case.suite.output / "cases" / case.case_id
            copied = destination / "private-base"
            self.assertEqual((copied / "stderr").read_bytes(), b"diagnostic bytes")
            self.assertTrue((copied / "unavailable-link").is_symlink())
            self.assertEqual((copied / "unavailable-link").readlink(), root / "not-present")
            self.assertTrue(case.base.exists())
            failure = json.loads((destination / "failure.json").read_text())
            self.assertTrue(failure["termination_unknown_preserved"])
            self.assertEqual(failure["unresolved_owned_processes"][0]["pid"], 123)

    def test_failed_case_is_reported_and_keeps_nonzero_exit(self):
        with tempfile.TemporaryDirectory() as temporary:
            suite = self.suite(temporary, "fail")
            stderr = io.StringIO()
            with patch("acceptance_supervisor_hermetic.digest", return_value="test-digest"), redirect_stderr(stderr):
                with self.assertRaises(SystemExit) as exit_result:
                    suite.run_targeted(["C07-locked-unknown"])
            self.assertEqual(exit_result.exception.code, 1)
            self.assertIn("FAIL C07-locked-unknown: injected assertion", stderr.getvalue())
            self.assertIn("injected traceback", stderr.getvalue())
            summary = json.loads((suite.output / "summary.json").read_text())
            self.assertEqual(summary["counts"], {"pass": 0, "fail": 1})
            self.assertEqual(summary["cases"], suite.results)

    def test_passing_case_does_not_emit_failure_diagnostics(self):
        with tempfile.TemporaryDirectory() as temporary:
            suite = self.suite(temporary, "pass")
            stderr, stdout = io.StringIO(), io.StringIO()
            with patch("acceptance_supervisor_hermetic.digest", return_value="test-digest"), redirect_stderr(stderr), redirect_stdout(stdout):
                suite.run_targeted(["C07-locked-unknown"])
            self.assertEqual(stderr.getvalue(), "")
            self.assertIn("PASS hermetic H targeted: 1 cases", stdout.getvalue())

    @staticmethod
    def suite(directory, status):
        suite = object.__new__(HermeticSuite)
        suite.tools = Path(directory)
        suite.output = Path(directory)
        suite.results = []

        def run_case(case_id, _function):
            suite.results.append({"case": case_id, "status": status,
                                  "error": "injected assertion", "traceback": "injected traceback"})

        suite.run_case = run_case
        return suite


class CaseFinalizationTests(unittest.TestCase):
    @staticmethod
    def fake_case(root, case_id, events):
        output = root / "output"
        output.mkdir()
        source = root / "private-base"
        source.mkdir()
        (source / "diagnostic.txt").write_text("source evidence\n")
        destination = output / "cases" / case_id
        case = object.__new__(Case)
        case.case_id = case_id
        case.base = source
        case.suite = SimpleNamespace(output=output)
        case.provider_release = None
        case.external_wait_required = False
        case.external_wait_observed = None
        case.external_wait_unknown = {}
        case.owned_directories = ("owned-token",)
        case.outcome = None
        case.processes = SimpleNamespace(
            drain=lambda **_kwargs: [],
            finished_naturally=lambda **_kwargs: True,
        )

        def copy_evidence():
            events.append("copy")
            destination.mkdir(mode=0o700, parents=True, exist_ok=True)
            (destination / "diagnostic.txt").write_text("diagnostic snapshot\n")
            return destination

        def receipt():
            events.append("receipt")
            return {"case": case_id}

        case.copy_evidence = copy_evidence
        case.receipt = receipt
        case.finish = lambda status="pass": Case.finish(case, status)
        case.retain_failure = lambda error: Case.retain_failure(case, error)
        return case, output, source, destination

    @staticmethod
    def suite(output):
        suite = object.__new__(HermeticSuite)
        suite.output = output
        suite.results = []
        return suite

    def test_unknown_external_wait_cannot_publish_or_pass(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            events = []
            case, output, source, destination = self.fake_case(root, "unknown-wait", events)
            case.external_wait_required = True
            case.external_wait_observed = False
            case.external_wait_unknown = {"helper-invocation": 701}
            case.suite = self.suite(output)
            suite = case.suite

            with patch.object(hermetic, "Case", return_value=case), \
                    patch.object(hermetic, "rollback_owned_directories") as rollback:
                suite.run_case(case.case_id, lambda _case: None)

            self.assertEqual(suite.results[0]["status"], "fail")
            self.assertNotIn("write-receipt", events)
            self.assertFalse((destination / "receipt.json").exists())
            self.assertTrue((destination / "failure.json").exists())
            self.assertTrue(source.exists())
            rollback.assert_not_called()

    def test_receipt_is_written_only_after_rollback(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            events = []
            case, _output, _source, _destination = self.fake_case(root, "ordering", events)

            def rollback(_tokens):
                events.append("rollback")
                return True

            def write_receipt(path, _value, **_kwargs):
                self.assertEqual(path.name, "receipt.json")
                events.append("write-receipt")

            with patch.object(hermetic, "rollback_owned_directories", side_effect=rollback), \
                    patch.object(hermetic, "write_json", side_effect=write_receipt):
                result = case.finish()

            self.assertEqual(result["status"], "pass")
            self.assertEqual(events, ["copy", "receipt", "rollback", "write-receipt"])

    def test_rollback_failure_is_a_case_failure_and_retains_source_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            events = []
            case, output, source, destination = self.fake_case(root, "rollback-failure", events)
            case.suite = self.suite(output)
            suite = case.suite

            def rollback(_tokens):
                events.append("rollback")
                raise RuntimeError("injected rollback failure")

            with patch.object(hermetic, "Case", return_value=case), \
                    patch.object(hermetic, "rollback_owned_directories", side_effect=rollback):
                suite.run_case(case.case_id, lambda _case: None)

            self.assertEqual(suite.results[0]["status"], "fail")
            self.assertIn("rollback failure", suite.results[0]["error"])
            self.assertFalse((destination / "receipt.json").exists())
            self.assertTrue((destination / "private-base" / "diagnostic.txt").exists())
            self.assertTrue(source.exists())
            self.assertEqual(events, ["copy", "receipt", "rollback"])


class A06RetryObservationTests(unittest.TestCase):
    def test_a06_waits_for_exact_retry_observer_before_counts(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            task_id = "b" * 32
            root_id = "a" * 32
            supervisor_records = root / "supervisor-records"
            supervisor_records.mkdir()
            task_dir = root / "tasks" / task_id
            task_dir.mkdir(parents=True)
            (task_dir / "supervisor.ref.json").write_text("{}\n")
            events = []
            add = {
                "verb": "add",
                "invocation_id": "add-invocation",
                "pid": 701,
                "argv": ["--label", "delegate:" + root_id + ":" + task_id],
            }
            first_enqueue = {
                "verb": "enqueue",
                "invocation_id": "enqueue-first",
                "pid": 702,
                "argv": ["-c", "config", "enqueue", "7"],
                "executable": "/pueue",
            }
            retry_enqueue = {
                "verb": "enqueue",
                "invocation_id": "enqueue-retry",
                "pid": 703,
                "argv": ["-c", "config", "enqueue", "7"],
                "executable": "/pueue",
            }

            class FakeProcess:
                def __init__(self, response):
                    self.response = response

                def wait(self, *_args, **_kwargs):
                    events.append("initial-dispatch-wait")

                def json(self):
                    return self.response

            class FakeCase:
                def __init__(self):
                    self.base = root
                    self.root = root
                    self.case_id = "A06"
                    self.task_id = task_id
                    self.root_id = None
                    self.supervisor_records = supervisor_records
                    self.enqueue_release = root / "enqueue.release"

                def update_fake(self, verb, **_changes):
                    self.updated_verb = verb

                def start_dispatch(self):
                    return FakeProcess({
                        "failure": {
                            "code": "task_launch_uncertain",
                            "stage": "enqueue-task",
                            "next_action": "check_status",
                        }
                    })

                def wait_entry(self, verb):
                    return add if verb == "add" else first_enqueue

                def set_status(self, state):
                    events.append("status:" + state)

                def release_add(self):
                    events.append("release-add")

                def wait_completion(self, entry):
                    events.append("helper:" + entry["invocation_id"])
                    return {"exit_code": 1 if entry is first_enqueue else 0}

                def observe_helper_completion_if_present(self, entry, timeout=1.0):
                    _ = timeout
                    completion = self.wait_completion(entry)
                    self.wait_supervisor_completion(entry)
                    return completion

                def wait_supervisor_completion(self, entry):
                    events.append("observer:" + entry["invocation_id"])

                def release(self, _path):
                    events.append("release-enqueue")

                def retry_dispatch(self, **_kwargs):
                    events.append("retry-dispatch")
                    return FakeProcess({"admission": "admitted"}), {"admission": "admitted"}

                def wait_entry_after(self, verb, previous_invocation_ids):
                    self.assertion = (verb, set(previous_invocation_ids))
                    events.append("retry-entry")
                    return retry_enqueue

                def counts(self):
                    events.append("counts")
                    return {"A": 1, "Q": 2, "S": 0, "E": 0, "K": 0, "M": 0}

            case = FakeCase()
            hermetic.run_a06(case)

            self.assertEqual(case.assertion, ("enqueue", {"enqueue-first"}))
            self.assertEqual(
                events[events.index("observer:enqueue-first"):],
                [
                    "observer:enqueue-first",
                    "release-enqueue",
                    "status:stashed",
                    "retry-dispatch",
                    "retry-entry",
                    "helper:enqueue-retry",
                    "observer:enqueue-retry",
                    "counts",
                ],
            )


class AcceptanceArtifactCleanupTests(unittest.TestCase):
    def test_failure_cleanup_budget_is_finite(self):
        with patch.object(supervisor_common.time, "monotonic",
                          side_effect=(10.0, 12.0, 16.0)), \
                patch.object(supervisor_common.time, "sleep") as sleep:
            deadline = supervisor_common.failure_cleanup_deadline(5)
            self.assertTrue(supervisor_common.wait_for_failure_cleanup(deadline, 10))
            self.assertFalse(supervisor_common.wait_for_failure_cleanup(deadline, 1))
        sleep.assert_called_once_with(3.0)

    def test_cleanup_preserves_shared_parent_unless_ownership_is_explicit(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            shared_parent = root / "shared"
            owned = shared_parent / "run"
            owned.mkdir(parents=True)
            owner = supervisor_common.DirectoryOwner()
            owner.adopt_created("state", owned, shared_parent)

            self.assertTrue(supervisor_common.finalize_acceptance_artifacts(
                output, True, owner.transfer(), True,
            ))
            self.assertTrue(shared_parent.is_dir())

            owned_parent = root / "owned-shared"
            owner = supervisor_common.DirectoryOwner()
            owner.create("owned parent", owned_parent, root)
            second = owned_parent / "run-2"
            owner.create("state", second, owned_parent)
            self.assertTrue(supervisor_common.finalize_acceptance_artifacts(
                output, True, owner.transfer(), True,
            ))
            self.assertFalse(owned_parent.exists())

    def test_completed_run_removes_only_its_owned_roots_and_raw_process_captures(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            (output / "summary.json").write_text("compact receipt\n")
            owner = supervisor_common.DirectoryOwner()
            captures = owner.create("raw process captures", output / "processes", output)
            (captures.directory / "001-provider").mkdir()
            (captures.directory / "001-provider" / "stdout").write_bytes(b"raw output")
            state_parent = root / "states"
            state = state_parent / "run"
            workspace_parent = root / "workspaces"
            workspace = workspace_parent / "run"
            state.mkdir(parents=True)
            workspace.mkdir(parents=True)
            owner.adopt_created("workspace", workspace, workspace_parent)
            owner.adopt_created("state", state, state_parent)
            tokens = owner.transfer()

            self.assertTrue(supervisor_common.finalize_acceptance_artifacts(
                output,
                True,
                tokens,
                True,
                raw_process_captures_token=captures,
            ))

            self.assertFalse(state.exists())
            self.assertFalse(workspace.exists())
            self.assertFalse((output / "processes").exists())
            self.assertEqual((output / "summary.json").read_text(), "compact receipt\n")
            receipt = json.loads((output / "cleanup.json").read_text())
            self.assertEqual(receipt["status"], "removed")
            self.assertEqual(receipt["removed"], ["state", "workspace", "raw process captures"])
            self.assertEqual(receipt["raw_process_captures"], "removed")

    def test_unclosed_supervisor_retains_all_test_artifacts(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            state_parent = root / "states"
            state = state_parent / "run"
            state.mkdir(parents=True)
            owner = supervisor_common.DirectoryOwner()
            captures = owner.create("raw process captures", output / "processes", output)
            owner.adopt_created("state", state, state_parent)

            self.assertFalse(supervisor_common.finalize_acceptance_artifacts(
                output, False, owner.transfer(), True,
                raw_process_captures_token=captures,
            ))

            self.assertTrue(state.is_dir())
            self.assertTrue((output / "processes").is_dir())
            receipt = json.loads((output / "cleanup.json").read_text())
            self.assertEqual(receipt["status"], "retained")
            self.assertFalse(receipt["supervisor_shutdown_observed"])

    def test_unowned_raw_process_captures_refuse_success_cleanup(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            captures = output / "processes"
            captures.mkdir()
            sentinel = captures / "pre-existing.txt"
            sentinel.write_text("owned by another run")
            state_parent = root / "states"
            state = state_parent / "run"
            state.mkdir(parents=True)
            owner = supervisor_common.DirectoryOwner()
            owner.adopt_created("state", state, state_parent)

            self.assertFalse(supervisor_common.finalize_acceptance_artifacts(
                output, True, owner.transfer(), True,
            ))

            self.assertTrue(state.exists())
            self.assertEqual(sentinel.read_text(), "owned by another run")
            self.assertFalse((output / "cleanup.json").exists())

    def test_failed_run_keeps_raw_process_captures_but_removes_closed_state(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            state_parent = root / "states"
            state = state_parent / "run"
            state.mkdir(parents=True)
            owner = supervisor_common.DirectoryOwner()
            captures = owner.create("raw process captures", output / "processes", output)
            owner.adopt_created("state", state, state_parent)

            self.assertTrue(supervisor_common.finalize_acceptance_artifacts(
                output, True, owner.transfer(), False,
                raw_process_captures_token=captures,
            ))

            self.assertFalse(state.exists())
            self.assertTrue((output / "processes").is_dir())
            receipt = json.loads((output / "cleanup.json").read_text())
            self.assertEqual(receipt["completion"], "failed")
            self.assertEqual(receipt["raw_process_captures"], "retained")

    def test_failed_run_can_remove_raw_captures_after_bounded_diagnostics(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            state_parent = root / "states"
            state = state_parent / "run"
            state.mkdir(parents=True)
            owner = supervisor_common.DirectoryOwner()
            captures = owner.create("raw process captures", output / "processes", output)
            (captures.directory / "001-provider").mkdir()
            (captures.directory / "001-provider" / "stdout").write_bytes(b"bounded receipt exists")
            owner.adopt_created("state", state, state_parent)

            self.assertTrue(supervisor_common.finalize_acceptance_artifacts(
                output, True, owner.transfer(), False,
                remove_failed_process_captures=True,
                raw_process_captures_token=captures,
            ))

            self.assertFalse(state.exists())
            self.assertFalse((output / "processes").exists())
            receipt = json.loads((output / "cleanup.json").read_text())
            self.assertEqual(receipt["completion"], "failed")
            self.assertEqual(receipt["raw_process_captures"], "removed")

    def test_cleanup_refuses_a_directory_that_contains_its_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "state" / "evidence"
            output.mkdir(parents=True)
            owner = supervisor_common.DirectoryOwner()
            owner.adopt_created("state", root / "state", root)

            self.assertFalse(supervisor_common.finalize_acceptance_artifacts(
                output, True, owner.transfer(), True,
            ))

            self.assertTrue(output.is_dir())

    def test_cleanup_refuses_a_symlinked_evidence_directory(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            target = root / "target"
            target.mkdir()
            output = root / "evidence"
            output.symlink_to(target, target_is_directory=True)

            self.assertFalse(supervisor_common.finalize_acceptance_artifacts(
                output, True, (), True,
            ))
            self.assertTrue(target.is_dir())


if __name__ == "__main__":
    unittest.main()
