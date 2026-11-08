"""Cleanup ownership checks for the CCR fixture gate."""

import argparse
from contextlib import nullcontext
from copy import deepcopy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import acceptance_ccr_fixture as gate
from acceptance_supervisor_common import DirectoryOwner
from scripts.acceptance_tests.resources import track_directory_pins
from scripts.delegate_acceptance.native import acceptance_lifecycle as lifecycle_impl
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.hermetic.contract import status_document, status_job
from scripts.delegate_acceptance.tasks.operations import AdmissionFailure, NativeTaskOps


class FixtureCleanupTests(unittest.TestCase):
    @staticmethod
    def fake_run(shutdown_result=True, finalize_result=True, finalize_error=None,
                 output=None):
        calls = []

        class Run:
            def __init__(self):
                self.output = output

            def capture_cpu_snapshot(self, _failure_observed_at):
                # This no-CPU fake has no refusal observation to diagnose.
                return None

            def safe_shutdown(self):
                calls.append("shutdown")
                return shutdown_result

            def finalize(self, success, *, shutdown_attempted=False):
                calls.append(("finalize", success, shutdown_attempted))
                if finalize_error is not None:
                    raise finalize_error
                return finalize_result

        return Run(), calls

    def test_success_finalizes_once_after_proven_shutdown(self):
        run, calls = self.fake_run()

        gate.cleanup_fixture(run, True)

        self.assertEqual(calls, ["shutdown", ("finalize", True, True)])

    def test_failure_still_finalizes_once_and_cleanup_failure_is_fatal(self):
        run, calls = self.fake_run(shutdown_result=False)

        with self.assertRaisesRegex(RuntimeError, "fixture cleanup failed"):
            gate.cleanup_fixture(run, False)

        self.assertEqual(calls, ["shutdown", ("finalize", False, True)])

    def test_finalize_failure_is_fatal(self):
        run, calls = self.fake_run(finalize_result=False)

        with self.assertRaisesRegex(RuntimeError, "fixture cleanup failed"):
            gate.cleanup_fixture(run, True)

        self.assertEqual(calls, ["shutdown", ("finalize", True, True)])

    def test_behavioral_success_is_not_published_when_finalization_fails(self):
        for failure in (False, OSError("injected finalization failure")):
            with self.subTest(failure=repr(failure)), \
                    tempfile.TemporaryDirectory(prefix="ccr-fixture-finalize-unit-") as temporary:
                output = Path(temporary) / "evidence"
                output.mkdir()
                run, calls = self.fake_run(
                    finalize_result=failure is not False,
                    finalize_error=failure if isinstance(failure, BaseException) else None,
                    output=output,
                )
                run.provider_executable = "/ccr"
                run.direct = lambda *_args, **_kwargs: None
                run.exercise = lambda _fixture: {"status": "passed", "behavior": "complete"}
                fixture = SimpleNamespace(server=SimpleNamespace(server_port=43210))

                class FixtureContext:
                    def __enter__(self):
                        return fixture

                    def __exit__(self, *_args):
                        return None

                def fixture_acceptance(_args, directories):
                    track_directory_pins(self, directories)
                    return run

                argv = ["acceptance_ccr_fixture.py", "--output", str(output)]
                with patch.object(gate, "choose_output", return_value=output), \
                        patch.object(gate, "FixtureAcceptance", side_effect=fixture_acceptance), \
                        patch.object(gate, "Fixture", return_value=FixtureContext()), \
                        patch.dict(gate.os.environ, {}, clear=False), \
                        patch.object(sys, "argv", argv):
                    with self.assertRaisesRegex(RuntimeError, "fixture cleanup failed"):
                        gate.main()

                self.assertEqual(calls, ["shutdown", ("finalize", True, True)])
                self.assertFalse((output / "success.json").exists())
                failure_receipt = json.loads((output / "failure.json").read_text())
                self.assertTrue(failure_receipt["behavior_completed"])
                self.assertTrue(failure_receipt["passed_receipt_absent"])
                cleanup = json.loads((output / "failure-cleanup.json").read_text())
                self.assertTrue(cleanup["natural_daemon_shutdown"])
                self.assertFalse(cleanup["artifact_finalization_completed"])
                self.assertTrue(cleanup["passed_receipt_absent"])

    def test_constructor_failure_rolls_back_fixture_profile(self):
        with tempfile.TemporaryDirectory(prefix="ccr-fixture-constructor-unit-") as temporary:
            output = Path(temporary) / "evidence"
            output.mkdir()
            owner = DirectoryOwner()
            profile_token = owner.create("CCR fixture profile", output / "profile", output)
            profile = profile_token.directory
            owner.create("CCR fixture data", profile_token.directory / "data", profile_token.directory)
            directories = owner.transfer()

            with patch.object(gate.NativeAcceptance, "__init__",
                              side_effect=RuntimeError("injected constructor failure")):
                with self.assertRaisesRegex(RuntimeError, "injected constructor failure"):
                    gate.FixtureAcceptance(SimpleNamespace(), directories)

            self.assertFalse(profile.exists())
            self.assertTrue(output.is_dir())

    def test_finalize_removes_fixture_profile_after_native_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="ccr-fixture-finalize-profile-unit-") as temporary:
            output = Path(temporary) / "evidence"
            output.mkdir()
            owner = DirectoryOwner()
            profile_token = owner.create("CCR fixture profile", output / "profile", output)
            profile = profile_token.directory
            run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
            run.fixture_directories = (profile_token,)

            with patch.object(gate.NativeAcceptance, "finalize", return_value=True):
                self.assertTrue(run.finalize(True))

            self.assertFalse(profile.exists())
            self.assertEqual(run.fixture_directories, ())

    def test_finalize_forwards_blocked_state_to_native_owner(self):
        run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
        run.fixture_directories = ()
        with patch.object(gate.NativeAcceptance, "finalize", return_value=True) as finalize:
            self.assertTrue(run.finalize(False, blocked=True))
        finalize.assert_called_once_with(False, blocked=True, shutdown_attempted=False)

    def test_finalize_retains_fixture_profile_until_native_cleanup(self):
        with tempfile.TemporaryDirectory(prefix="ccr-fixture-retain-profile-unit-") as temporary:
            output = Path(temporary) / "evidence"
            output.mkdir()
            owner = DirectoryOwner()
            profile_token = owner.create("CCR fixture profile", output / "profile", output)
            track_directory_pins(self, (profile_token,))
            profile = profile_token.directory
            run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
            run.fixture_directories = (profile_token,)

            with patch.object(gate.NativeAcceptance, "finalize", return_value=False):
                self.assertFalse(run.finalize(False))

            self.assertTrue(profile.is_dir())



class RecordingFixture(gate.Fixture):
    """The real local HTTP fixture, recorded so the CLI stand-in can mark its events."""

    instances: list["RecordingFixture"] = []

    def __init__(self):
        super().__init__()
        RecordingFixture.instances.append(self)


ROOT_ID = "f" * 32


def user_cancellation(task_id):
    """Delegate's closed user-cancellation collect envelope: terminal interruption, exit 1."""
    return 1, {
        "schema_version": 1, "command": "collect", "root_id": ROOT_ID, "task_id": task_id,
        "status": "interrupted", "admission": "unknown", "liveness": "undetermined",
        "publication": "unknown",
        "continuation": {"resumable": False, "mode": "native", "predecessor_task_id": task_id,
                         "session_source": "observed", "reason": "task was cancelled by the user"},
        "failure": {"code": "task_interrupted", "stage": "collect", "next_action": "stop_and_report"},
        "error": "task was interrupted before it published an outcome",
    }


def budget_timeout(task_id):
    """A budget-only stop: timed out with publication still pending, exit 3."""
    return 3, {"schema_version": 1, "command": "collect", "root_id": ROOT_ID, "task_id": task_id,
               "status": "timed_out", "pending": {"kind": "publication"}}


class FixturePublicationTests(unittest.TestCase):
    """The real exercise, cleanup, fixture finalizer and shared publisher; only CLIs are stubbed."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="ccr-fixture-publication-unit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.output = self.root / "evidence"
        self.output.mkdir()
        RecordingFixture.instances = []
        self.collect_responses = {"cancel-collect": user_cancellation, "budget-collect": budget_timeout}

    def closed_run(self) -> gate.FixtureAcceptance:
        owner = DirectoryOwner()
        base = owner.create("acceptance base", self.root / "private", self.root)
        state = owner.create("acceptance state", base.directory / "state", base.directory)
        workspace = owner.create("acceptance workspace", base.directory / "workspace", base.directory)
        briefs = owner.create("acceptance briefs", base.directory / "briefs", base.directory)
        raw = owner.create("raw process captures", self.output / "processes", self.output)
        (raw.directory / "001-provider").mkdir()
        (raw.directory / "001-provider" / "stdout").write_text("raw provider capture\n")
        profile_owner = DirectoryOwner()
        profile = profile_owner.create("CCR fixture profile", self.output / "profile", self.output)
        profile_owner.create("CCR fixture data", profile.directory / "data", profile.directory)
        run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
        run.owned_directories = owner.transfer()
        run.fixture_directories = profile_owner.transfer()
        track_directory_pins(self, run.owned_directories + run.fixture_directories)
        run.output, run.base, run.state = self.output, base.directory, state.directory
        run.workspace, run.briefs = workspace.directory, briefs.directory
        run.processes = object.__new__(Processes)
        run.processes.capture_root = raw
        run.closed, run.success_candidate = True, None
        run.args, run.mode = argparse.Namespace(model="ok"), "read-only"
        run.tasks, run.numeric_ids = {}, {}
        run.root_id = ROOT_ID
        run.provider_executable = Path("/ccr")
        self.install_cli(run)
        return run

    def install_cli(self, run: gate.FixtureAcceptance) -> None:
        """Finite stand-ins for the Delegate, CCR and supervisor CLI calls."""
        sessions: dict[str, str] = {}

        def dispatch(name, _brief, predecessor=None):
            task_id = f"{len(run.tasks) + 1:032x}"
            run.tasks[name], run.numeric_ids[task_id] = task_id, len(run.tasks) + 1
            sessions[task_id] = sessions[predecessor] if predecessor else "session-" + name
            directory = run.state / "tasks" / task_id
            directory.mkdir(parents=True)
            for record in ("outcome.json", "provider.exit"):
                (directory / record).write_text("{}\n")
            if name in RecordingFixture.instances[-1].seen:
                (directory / "stop").mkdir()
                RecordingFixture.instances[-1].seen[name].set()
                RecordingFixture.instances[-1].disconnected[name].set()
            else:
                (directory / "result.txt").write_text("CCR_FIXTURE_OK\n")
            if name == "written":
                for target, text in (("fixture-new.txt", "edited"), ("fixture-existing.txt", "changed"),
                                     ("fixture-shell.txt", "edited")):
                    (run.workspace / target).write_text(text)
            return task_id

        def command(name, *args, expected=0):
            task_id = args[1] if args[:1] == ("collect",) else None
            if name in self.collect_responses:
                exit_code, response = self.collect_responses[name](task_id)
            else:
                exit_code = 4 if name == "missing-collect" else 0
                response = {"command": args[0], "root_id": ROOT_ID, "task_id": task_id,
                            "publication": "rejected" if exit_code == 4 else "committed"}
            # The real process runner refuses any exit outside the caller's expected contract.
            if exit_code != expected:
                raise RuntimeError(f"unexpected exit {exit_code} for {name}")
            return SimpleNamespace(json=lambda: response, result={"exit_code": exit_code})

        def client(name, _args):
            capture = run.output / "processes" / name
            capture.mkdir()
            rows = [status_job(number, "fixture:" + str(number), "ended")
                    for number in run.numeric_ids.values()]
            (capture / "stdout").write_text(json.dumps(status_document(rows)))
            return SimpleNamespace(directory=capture, result={"exit_code": 0, "natural_wait": True})

        patched_cli = patch.multiple(
            run, create=True, direct=lambda *_args, **_kwargs: None, setup=lambda: None,
            ops=SimpleNamespace(client=client, dispatch_observations=[]), dispatch=dispatch, command=command,
            wait_done=lambda *_args: None, session_ref=lambda task_id: sessions[task_id])
        patched_cli.start()
        self.addCleanup(patched_cli.stop)

    def main(self, run: gate.FixtureAcceptance, *patches, refusal_cleanup=False):
        argv = ["acceptance_ccr_fixture.py", "--output", str(self.output)]
        if refusal_cleanup:
            argv.append("--refusal-cleanup")
        with patch.object(gate, "choose_output", return_value=self.output), \
                patch.object(gate, "prepare_fixture_acceptance", return_value=run), \
                patch.object(gate, "Fixture", RecordingFixture), \
                patch.object(sys, "argv", argv):
            for active in patches:
                active.start()
            try:
                return gate.main()
            finally:
                for active in reversed(patches):
                    active.stop()

    def test_failed_shutdown_is_not_repeated_by_real_finalizer(self):
        run = self.closed_run()
        run.closed = False
        (self.output / "success.json").write_text('{"status":"passed"}\n')

        with patch.object(run, "safe_shutdown", return_value=False) as shutdown, \
                patch.object(run, "_write_failure_diagnostics", return_value=True):
            with self.assertRaisesRegex(RuntimeError, "private supervisor cleanup was not proven"):
                gate.cleanup_fixture(run, True)

        shutdown.assert_called_once_with()
        self.assertFalse(run.closed)
        self.assertTrue(run.base.is_dir())
        self.assertTrue((self.output / "profile").is_dir())
        self.assertTrue((self.output / "processes" / "001-provider" / "stdout").is_file())
        self.assertFalse((self.output / "success.json").exists())
        self.assertIsNone(run.success_candidate)
        cleanup = json.loads((self.output / "cleanup.json").read_text())
        self.assertEqual(cleanup["status"], "retained")
        self.assertFalse(cleanup["supervisor_shutdown_observed"])

    def test_shutdown_exception_is_not_repeated_by_real_finalizer(self):
        run = self.closed_run()
        run.closed = False

        with patch.object(run, "safe_shutdown", side_effect=OSError("shutdown observation failed")) as shutdown, \
                patch.object(run, "_write_failure_diagnostics", return_value=True):
            with self.assertRaisesRegex(RuntimeError, "shutdown observation failed") as raised:
                gate.cleanup_fixture(run, False)

        shutdown.assert_called_once_with()
        self.assertIsInstance(raised.exception.__cause__, OSError)
        self.assertTrue(run.base.is_dir())
        self.assertTrue((self.output / "profile").is_dir())
        self.assertFalse((self.output / "success.json").exists())
        self.assertFalse(run.closed)

    def test_standalone_real_finalizer_still_attempts_shutdown(self):
        run = self.closed_run()
        run.closed = False

        with patch.object(run, "safe_shutdown", return_value=False) as shutdown, \
                patch.object(run, "_write_failure_diagnostics", return_value=True):
            self.assertFalse(run.finalize(False))

        shutdown.assert_called_once_with()
        self.assertTrue(run.base.is_dir())
        self.assertTrue((self.output / "profile").is_dir())
        self.assertFalse(run.closed)

    def test_refusal_mode_publishes_its_own_gate_after_real_finalization(self):
        run = self.closed_run()
        response = cpu_refusal_observation()["response"]
        expected: dict[str, object] = {"gate": "acceptance-ccr-refusal-cleanup", "status": "passed",
                                      "original_failure": {"status": "failed", "response": response}}
        run.success_receipt = expected
        with patch.object(run, "exercise", return_value=expected):
            self.assertEqual(self.main(run, refusal_cleanup=True), 0)
        self.assertEqual(json.loads((self.output / "success.json").read_text()), expected)
        self.assertFalse(run.base.exists())
        self.assertFalse((self.output / "profile").exists())

    def test_success_publishes_the_confirmed_candidate_only_after_all_cleanup(self):
        run = self.closed_run()
        profile = self.output / "profile"
        observed = []
        real_rollback = gate.rollback_owned_directories

        def rollback_after_native_cleanup(directories):
            observed.append(((self.output / lifecycle_impl.SUCCESS_CANDIDATE_NAME).is_file(),
                             (self.output / "success.json").exists(), run.base.exists()))
            return real_rollback(directories)

        self.assertEqual(self.main(run, patch.object(gate, "rollback_owned_directories",
                                                     side_effect=rollback_after_native_cleanup)), 0)
        self.assertEqual(observed, [(True, False, False)])
        data = (self.output / lifecycle_impl.SUCCESS_CANDIDATE_NAME).read_bytes()
        self.assertEqual((self.output / "success.json").read_bytes(), data)
        candidate = run.success_candidate
        self.assertIsNotNone(candidate)
        if candidate is None:
            self.fail("successful run did not commit its candidate")
        self.assertTrue(candidate.confirmed)
        self.assertEqual(candidate.sha256, hashlib.sha256(data).hexdigest())
        receipt = json.loads(data)
        self.assertEqual((receipt["gate"], receipt["status"]), (gate.FIXTURE_GATE, "passed"))
        self.assertEqual(receipt["tasks"], run.tasks)
        self.assertEqual(len(run.tasks), 8)
        self.assertFalse(profile.exists())
        self.assertFalse((self.output / "processes").exists())
        cleanup = json.loads((self.output / "cleanup.json").read_text())
        self.assertEqual((cleanup["status"], cleanup["completion"]), ("removed", "passed"))

    def test_failed_cleanup_never_publishes_and_keeps_the_committed_candidate(self):
        for label, fault in (
                ("fixture-profile", patch.object(gate, "rollback_owned_directories",
                                                 side_effect=OSError("profile cleanup failed"))),
                ("native-artifacts", patch.object(lifecycle_impl, "finalize_acceptance_artifacts",
                                                  return_value=False))):
            with self.subTest(fault=label):
                self.setUp()
                run = self.closed_run()
                with self.assertRaisesRegex(RuntimeError, "fixture cleanup failed"):
                    self.main(run, fault)
                self.assertFalse((self.output / "success.json").exists())
                candidate = json.loads((self.output / lifecycle_impl.SUCCESS_CANDIDATE_NAME).read_text())
                self.assertEqual(candidate, run.success_receipt)
                failure = json.loads((self.output / "failure.json").read_text())
                self.assertTrue(failure["behavior_completed"])
                self.assertTrue(failure["passed_receipt_absent"])
                self.assertTrue((self.output / "profile").is_dir())

    def test_user_cancellation_collect_is_a_terminal_interruption(self):
        run = self.closed_run()
        observed = []

        def recorded(task_id):
            observed.append(task_id)
            return user_cancellation(task_id)

        self.collect_responses["cancel-collect"] = recorded
        self.assertEqual(self.main(run), 0)
        self.assertEqual(observed, [run.tasks["cancel"]])
        self.assertTrue((self.output / "success.json").is_file())

    def test_cancel_collect_rejects_any_other_envelope(self):
        def changed(exit_code=1, **fields):
            def response(task_id):
                _, envelope = user_cancellation(task_id)
                for key, value in fields.items():
                    if value is None:
                        envelope.pop(key)
                    else:
                        envelope[key] = value
                return exit_code, envelope
            return response

        def changed_continuation(**fields):
            # Mutate only the named fields of this task's own valid continuation.
            def response(task_id):
                exit_code, envelope = user_cancellation(task_id)
                envelope["continuation"] = {**envelope["continuation"], **fields}
                return exit_code, envelope
            return response

        cases = {
            "unrelated-exit-1": ("cancel-collect", lambda task_id: (1, {
                "command": "collect", "root_id": ROOT_ID, "task_id": task_id,
                "error": "collect failed"}), "unpublished interruption"),
            "cancelled-pending-exit-3": ("cancel-collect", changed(
                3, status="timed_out", publication=None, pending={"kind": "publication"},
                failure={"code": "task_cancelled", "stage": "collect",
                         "next_action": "stop_and_report"}), "unexpected exit 3"),
            "task-cancelled-failure": ("cancel-collect", changed(failure={
                "code": "task_cancelled", "stage": "collect", "next_action": "stop_and_report"}),
                "task_interrupted/stop_and_report"),
            "continue-next-action": ("cancel-collect", changed(failure={
                "code": "task_interrupted", "stage": "collect", "next_action": "continue_task"}),
                "task_interrupted/stop_and_report"),
            "resumable": ("cancel-collect", changed_continuation(resumable=True),
                          "automatic continuation"),
            "other-reason": ("cancel-collect", changed_continuation(reason="task was interrupted"),
                             "automatic continuation"),
            "outcome-present": ("cancel-collect", changed(outcome={"status": "ok"}),
                                "unpublished interruption"),
            "other-task": ("cancel-collect", changed(task_id="0" * 32), "changed task identity"),
            "budget-exit-1": ("budget-collect", user_cancellation, "unexpected exit 1"),
        }
        for label, (name, response, message) in cases.items():
            with self.subTest(case=label):
                self.setUp()
                self.collect_responses[name] = response
                run = self.closed_run()
                with self.assertRaisesRegex(RuntimeError, message):
                    self.main(run)
                self.assertFalse((self.output / "success.json").exists())
                failure = json.loads((self.output / "failure.json").read_text())
                self.assertFalse(failure["behavior_completed"])
                self.assertIn(message, failure["error"])

    def test_success_finalization_without_a_receipt_is_refused_not_an_attribute_error(self):
        run = self.closed_run()
        self.assertFalse(run.finalize(True))
        self.assertFalse((self.output / lifecycle_impl.SUCCESS_CANDIDATE_NAME).exists())
        self.assertFalse((self.output / "success.json").exists())
        self.assertTrue(run.base.is_dir())
        self.assertTrue((self.output / "profile").is_dir())
        with self.assertRaisesRegex(lifecycle_impl.AcceptanceFailure, "candidate was not committed"):
            run.publish_success(expected_gate=gate.FIXTURE_GATE)


class FixtureWorkflowTests(unittest.TestCase):
    def test_provider_lock_covers_each_supported_native_platform(self):
        directory = Path(__file__).resolve().parents[1] / "tools/ccr-provider"
        manifest = json.loads((directory / "package.json").read_text())
        locked = json.loads((directory / "package-lock.json").read_text())
        self.assertTrue(manifest["private"])
        self.assertEqual(manifest["dependencies"], {"@anthropic-ai/claude-code": "2.1.318"})
        self.assertEqual(manifest["allowScripts"], {"@anthropic-ai/claude-code@2.1.318": True})
        packages = locked["packages"]
        self.assertEqual(packages[""]["dependencies"], manifest["dependencies"])
        wrapper = packages["node_modules/@anthropic-ai/claude-code"]
        self.assertTrue(wrapper["hasInstallScript"])
        self.assertEqual(wrapper["version"], "2.1.318")
        for platform in ("darwin-arm64", "darwin-x64", "linux-arm64", "linux-x64"):
            with self.subTest(platform=platform):
                name = f"@anthropic-ai/claude-code-{platform}"
                self.assertEqual(wrapper["optionalDependencies"][name], wrapper["version"])
                native = packages[f"node_modules/{name}"]
                self.assertTrue(native["optional"])
                self.assertEqual(native["version"], wrapper["version"])
                self.assertEqual(native["os"], [platform.split("-")[0]])
                self.assertEqual(native["cpu"], [platform.split("-")[1]])
                self.assertRegex(native["integrity"], r"^sha512-[A-Za-z0-9+/]{86}==$")
                self.assertTrue(native["resolved"].startswith("https://registry.npmjs.org/"))

    def test_ci_retains_only_existing_public_control_records(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/ccr-integration.yml"
        upload = workflow.read_text().split("      - name: Retain sanitized result\n", 1)[1]
        section = upload.split("          path:", 1)[1].split("          if-no-files-found:", 1)[0]
        paths = {line.strip() for line in section.splitlines() if line.strip() not in ("", "|")}
        root = "~/ccr-integration-evidence"
        expected = {f"{root}/{name}" for name in (
            "success.json", "*-dispatch-attempt.json", "*-continue-attempt.json",
            "failure-diagnostics.json", "failure-host-cpu-snapshot.json", "failure-cleanup.json", "failure-cleanup-expired.json")}
        expected.update(f"~/ccr-refusal-cleanup-evidence/{name}" for name in (
            "success.json", "*-dispatch-attempt.json", "failure-diagnostics.json",
            "failure-cleanup.json", "failure-cleanup-expired.json", "cleanup.json"))
        self.assertEqual(paths, expected)
        self.assertIn("if: always()", upload)

    def test_native_cpu_probe_precedes_real_ccr_consumer_in_same_step(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/ccr-integration.yml"
        step = workflow.read_text().split("      - name: Prove foreground lifecycle using real CLIs\n", 1)[1].split("      - name:", 1)[0]
        probe = "python3 -m unittest discover -s scripts -p test_acceptance_ccr_fixture.py -k native_cpu_snapshot -v"
        consumer = "python3 scripts/acceptance_ccr_fixture.py --output"
        refusal = "python3 scripts/acceptance_ccr_fixture.py --refusal-cleanup --output"
        self.assertEqual(step.count(probe), 1)
        self.assertEqual(step.count(refusal), 1)
        self.assertLess(step.index(probe), step.index(consumer))
        self.assertLess(step.index(consumer), step.index(refusal))
        loader = unittest.TestLoader()
        loader.testNamePatterns = ["*native_cpu_snapshot*"]
        self.assertEqual(loader.loadTestsFromModule(sys.modules[__name__]).countTestCases(), 1)


def cpu_refusal_observation():
    task_id = "c" * 32
    return {"task_id": task_id, "natural_wait": True, "exit_code": 1, "response": {
        "schema_version": 1, "command": "dispatch", "root_id": ROOT_ID, "task_id": task_id,
        "task_record": "unknown", "admission": "unknown", "liveness": "undetermined", "publication": "unknown",
        "failure": {"code": "resource_pressure", "stage": "check-resources",
                    "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"},
        "resource_admission": {"status": "pressure"},
    }}


class ControlledRefusalTests(unittest.TestCase):
    def test_finite_injection_uses_actual_capture_wait_and_keeps_unknown_refusal(self):
        base = Path(tempfile.mkdtemp(prefix="ccr-controlled-refusal-")).resolve(strict=True)
        owner = DirectoryOwner()
        owner.adopt_created("controlled refusal test", base, base.parent)
        processes = None

        def close_owned():
            try:
                if processes is not None and processes.drain(timeout=5):
                    raise RuntimeError("controlled refusal command closure unproven")
            except BaseException:
                owner.release()
                raise
            owner.rollback()

        self.addCleanup(close_owned)
        processes = Processes(base / "raw", {}, owner=owner)
        run = gate.RefusalCleanupAcceptance.__new__(gate.RefusalCleanupAcceptance)
        run.inject_refusal, run.refused_task_id, run.root_id = True, None, ROOT_ID
        task = "c" * 32
        argv = run.dispatch_argv(task, base / "brief")
        self.assertEqual(Path(argv[0]), Path(sys.executable))
        ops = NativeTaskOps(processes, Path("/delegate"), Path("/delegate-run"), Path("/pueue"),
                            base, base / "state", base)
        ops.root_id = ROOT_ID
        with self.assertRaises(AdmissionFailure) as caught:
            ops.dispatch("refused", task, argv)
        self.assertEqual(len(processes.entries), 1)
        self.assertEqual(processes.entries[0].result["exit_code"], 1)
        self.assertTrue(processes.entries[0].result["natural_wait"])
        observation = ops.dispatch_observations[0]
        self.assertTrue(observation["receipt_written"])
        self.assertTrue(gate.confirmed_host_cpu_refusal(observation, ROOT_ID))
        response = dict(caught.exception.response)
        self.assertEqual(response["task_record"], "unknown")
        self.assertEqual(response["failure"], {
            "code": "resource_pressure", "stage": "check-resources",
            "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"})
        self.assertEqual(observation["response"], response)
        self.assertFalse(ops.tasks)
        self.assertEqual(run.refused_task_id, task)
        run.inject_refusal = True
        with self.assertRaisesRegex(RuntimeError, "only one dispatch"):
            run.dispatch_argv(task, base / "brief")

    def test_genuine_warm_refusal_is_not_the_expected_negative(self):
        with tempfile.TemporaryDirectory(prefix="ccr-warm-refusal-") as temporary:
            run = gate.RefusalCleanupAcceptance.__new__(gate.RefusalCleanupAcceptance)
            run.briefs, run.args = Path(temporary), argparse.Namespace(model="ok")
            run.inject_refusal, run.refused_task_id = False, None
            error = AdmissionFailure("warm resource refusal", cpu_refusal_observation()["response"])
            with patch.object(run, "setup"), patch.object(run, "dispatch", side_effect=error) as dispatch, \
                    patch.object(run, "published") as published:
                with self.assertRaises(AdmissionFailure) as caught:
                    run.exercise(None)
            self.assertIs(caught.exception, error)
            dispatch.assert_called_once_with("warm", run.briefs / "refusal-cleanup.txt")
            published.assert_not_called()
            self.assertFalse(run.inject_refusal)
            self.assertIsNone(run.refused_task_id)
            self.assertIsNone(run.success_receipt)

    def test_unknown_task_control_prevents_a_passed_cleanup_receipt(self):
        run = gate.RefusalCleanupAcceptance.__new__(gate.RefusalCleanupAcceptance)
        task = "c" * 32
        run.refused_task_id, run.observed_absence = task, frozenset((task,))
        run.success_receipt, run.closed, run.delegate = {}, True, Path("/delegate")
        with patch.object(gate.FixtureAcceptance, "safe_shutdown", return_value=True), \
                patch.object(run, "processes", create=True,
                             entries=[SimpleNamespace(argv=["/delegate", "status", task])]), \
                patch.object(gate.acceptance_lifecycle, "record_lifetime_closure") as closure:
            with self.assertRaisesRegex(RuntimeError, "refused task reached another Delegate"):
                run.safe_shutdown()
        closure.assert_not_called()
        self.assertEqual(run.success_receipt, {})


class CPUSnapshotTests(unittest.TestCase):
    def test_projection_preserves_spaces_and_public_roles_without_unknown_names(self):
        projected = gate.cpu_snapshot_rows(
            "150.5 /Applications/Private Canary/claude\n"
            "20.5 /private/token-canary/unknown-secret\n"
            "7.0 /System/with spaces/mdworker_shared\n"
            "4.5 /System/XprotectService\n3 /bin/ccr\n2 /other/ccr\n")
        self.assertEqual(projected, {
            "listed_process_count": {"claude": 1, "other": 1, "mdworker_shared": 1, "xprotectservice": 1, "ccr": 2},
            "top_cpu": [{"role": role, "native_cpu_percent": cpu} for role, cpu in (
                ("claude", 150.5), ("other", 20.5), ("mdworker_shared", 7.0), ("xprotectservice", 4.5), ("ccr", 3.0), ("ccr", 2.0))],
        })
        encoded = json.dumps(projected)
        for secret in ("Private Canary", "token-canary", "unknown-secret", "/", "pid", "live", "leaked"):
            self.assertNotIn(secret, encoded)

    def test_public_runner_security_names_are_case_insensitive_and_counts_are_only_listed(self):
        projected = gate.cpu_snapshot_rows("4 /runner/Runner.Worker\n3 /runner/RUNNER.LISTENER\n"
                                           "2 /system/TRUSTD\n1 /system/XProtect\n0.5 ccr <defunct>\n")
        self.assertEqual(projected["listed_process_count"], {
            "runner.worker": 1, "runner.listener": 1, "trustd": 1, "xprotect": 1, "other": 1})
        self.assertEqual(projected["top_cpu"], [{"role": role, "native_cpu_percent": cpu} for role, cpu in (
            ("runner.worker", 4.0), ("runner.listener", 3.0), ("trustd", 2.0), ("xprotect", 1.0), ("other", 0.5))])

    def test_all_rows_are_validated_and_sorted_before_the_top_eight(self):
        projected = gate.cpu_snapshot_rows("".join(f"{i} private-{i}\n" for i in range(12)) + "240 /bin/ccr\n")
        self.assertEqual(projected["listed_process_count"], {"other": 12, "ccr": 1})
        self.assertEqual(projected["top_cpu"], [{"role": "ccr", "native_cpu_percent": 240.0}] +
                         [{"role": "other", "native_cpu_percent": float(i)} for i in (11, 10, 9, 8, 7, 6, 5)])

    def test_empty_malformed_nonfinite_negative_partial_and_bounded_inputs_are_refused(self):
        for text in ("", "\n", "bad ps\n", "nan ps\n", "inf ps\n", "1e999 ps\n", "-0.1 ps\n", "1,5 ps\n",
                     "12\n", "1 /\n", "1 ccr\nbroken\n", "0 ps\n" * 4097, "0 " + "x" * (256 * 1024)):
            with self.subTest(prefix=text[:20]), self.assertRaises(ValueError):
                gate.cpu_snapshot_rows(text)

    def test_only_exact_completed_host_cpu_refusal_is_eligible(self):
        observation = cpu_refusal_observation()
        self.assertTrue(gate.confirmed_host_cpu_refusal(observation, ROOT_ID))
        self.assertTrue(gate.confirmed_host_cpu_refusal(observation, None))
        self.assertFalse(gate.confirmed_host_cpu_refusal(observation, "invalid-known-root"))
        for target, key, value in (
                ("observation", "natural_wait", False), ("observation", "exit_code", 0), ("observation", "exit_code", True),
                ("observation", "task_id", "invalid"), ("response", "root_id", "e" * 32),
                ("response", "task_id", "d" * 32), ("response", "schema_version", True),
                ("response", "command", "status"), ("response", "admission", "admitted"),
                ("response", "resource_admission", {"status": "unknown"}),
                ("response", "failure", {"code": "resource_pressure", "stage": "check-resources",
                                           "reason_code": "host_memory_critical", "next_action": "stop_and_report"})):
            with self.subTest(target=target, key=key):
                changed = deepcopy(observation)
                container = changed if target == "observation" else changed["response"]
                container[key] = value
                self.assertFalse(gate.confirmed_host_cpu_refusal(changed, ROOT_ID))
        observation["response"]["root_id"] = "invalid-response-root"
        self.assertFalse(gate.confirmed_host_cpu_refusal(observation, None))

    def test_ineligible_observations_never_sample_or_write_a_snapshot(self):
        for changes in ({"resource_admission": {"status": "unknown"}}, {"admission": "admitted"},
                        {"failure": {"code": "resource_pressure", "stage": "check-resources",
                                     "reason_code": "host_memory_critical", "next_action": "stop_and_report"}}):
            with self.subTest(changes=changes), tempfile.TemporaryDirectory(prefix="ccr-no-cpu-") as temporary:
                output = Path(temporary)
                observation = cpu_refusal_observation()
                observation["response"].update(changes)
                run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
                with patch.multiple(run, create=True, output=output, root_id=ROOT_ID,
                                    ops=SimpleNamespace(dispatch_observations=[observation])), \
                        patch.object(run, "processes", create=True) as processes:
                    run.capture_cpu_snapshot(time.monotonic())
                processes.run.assert_not_called()
                self.assertFalse((output / gate.CPU_SNAPSHOT_NAME).exists())

    def test_native_cpu_snapshot_compatibility(self):
        # This is the one native prerequisite selected before the CCR consumer.
        # The process registry alone owns ps Start/Wait; raw names remain private.
        base = Path(tempfile.mkdtemp(prefix="ccr-native-ps-")).resolve(strict=True)
        owner = DirectoryOwner()
        owner.adopt_created("native ps test", base, base.parent)
        processes = None

        def close_owned():
            try:
                if processes is not None and processes.drain(timeout=5):
                    raise RuntimeError("native ps closure unproven; retaining owned capture")
            except BaseException:
                owner.release()
                raise
            owner.rollback()

        self.addCleanup(close_owned)
        processes = Processes(base / "raw", {"LC_ALL": "C"}, owner=owner)
        run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
        with patch.multiple(run, create=True, output=base, root_id=None, processes=processes,
                            ops=SimpleNamespace(dispatch_observations=[cpu_refusal_observation()])):
            run.capture_cpu_snapshot(time.monotonic())
            self.assertIsNone(run.root_id)
        self.assertEqual(len(processes.entries), 1)
        process = processes.entries[0]
        self.assertEqual(process.argv, ["/bin/ps", "-A", "-o", "pcpu=,comm="])
        self.assertEqual(process.environment, {"LC_ALL": "C"})
        self.assertTrue(process.result["natural_wait"])
        self.assertEqual(process.result["exit_code"], 0)
        receipt = json.loads((base / gate.CPU_SNAPSHOT_NAME).read_text())
        self.assertEqual(receipt["status"], "observed")
        self.assertTrue(receipt["listed_process_count"])
        self.assertGreater(len(receipt["top_cpu"]), 0)
        self.assertLessEqual(len(receipt["top_cpu"]), 8)
        self.assertTrue(set(receipt["listed_process_count"]) <= gate.CPU_SNAPSHOT_ROLES | {"other"})
        self.assertFalse(receipt["admission_intervals_matched"])
        self.assertFalse(receipt["causal_attribution"])

    def test_capture_offsets_bracket_delayed_ps_and_bounded_read(self):
        with tempfile.TemporaryDirectory(prefix="ccr-cpu-offset-") as temporary:
            output = Path(temporary)
            clock = [10.125]
            process = SimpleNamespace(directory=output, result={"natural_wait": True, "exit_code": 0})

            def ps(*_args, **_options):
                clock[0] = 12.125
                return process

            def read(*_args):
                clock[0] = 12.5
                return "123.5 /private/ccr\n"

            run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
            with patch.multiple(run, create=True, output=output, root_id=ROOT_ID,
                                processes=SimpleNamespace(run=ps, environment={"LC_ALL": "C"}),
                                ops=SimpleNamespace(dispatch_observations=[cpu_refusal_observation()])), \
                    patch.object(gate.time, "monotonic", side_effect=lambda: clock[0]), \
                    patch.object(gate, "bounded_text", side_effect=read) as bounded_read:
                run.capture_cpu_snapshot(10.0)
            bounded_read.assert_called_once_with(output / "stdout", "native CPU listing", 256 * 1024)
            receipt = json.loads((output / gate.CPU_SNAPSHOT_NAME).read_text())
            self.assertEqual(receipt["origin"], "driver_failure_observed")
            self.assertEqual(receipt["capture_started_offset_ms"], 125)
            self.assertEqual(receipt["capture_completed_offset_ms"], 2500)
            self.assertNotIn("offset_ms", receipt)

    def test_diagnostic_faults_keep_primary_and_precede_unchanged_cleanup(self):
        for fault in ("capture", "interrupt", "completion", "exit", "read", "parse", "write", "none"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory(prefix="ccr-cpu-unit-") as temporary:
                output = Path(temporary)
                raw = output / "raw"
                raw.mkdir()
                (raw / "stdout").write_text("NaN /private/canary\n" if fault == "parse" else "123.5 /private/ccr\n")
                primary = RuntimeError("original admission failure")
                order = []

                def ps(name, argv, cwd, **options):
                    order.append("snapshot")
                    self.assertEqual((name, argv, cwd, options), (
                        "failure-host-cpu", ["/bin/ps", "-A", "-o", "pcpu=,comm="], output,
                        {"expected": None, "timeout": 5, "environment": {"LC_ALL": "C", "KNOWN": "fixture"}}))
                    if fault == "capture":
                        raise OSError("private canary /secret PID=4242")
                    if fault == "interrupt":
                        raise KeyboardInterrupt()
                    return SimpleNamespace(directory=raw / "missing" if fault == "read" else raw,
                                           result={"natural_wait": fault != "completion",
                                                   "exit_code": 1 if fault == "exit" else 0})

                def exercise(_fixture):
                    raise primary

                real_write = gate.write_json

                def write(path, value, **options):
                    if path.name == gate.CPU_SNAPSHOT_NAME and fault == "write":
                        raise OSError("private receipt canary")
                    return real_write(path, value, **options)

                run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
                environment = {"LC_ALL": "de_DE", "KNOWN": "fixture"}
                fixture = SimpleNamespace(server=SimpleNamespace(server_port=43210))
                with patch.multiple(run, create=True, output=output, root_id=ROOT_ID,
                                    processes=SimpleNamespace(run=ps, environment=environment),
                                    ops=SimpleNamespace(dispatch_observations=[cpu_refusal_observation()]),
                                    provider_executable="/ccr", direct=lambda *_args, **_kwargs: None,
                                    exercise=exercise), \
                        patch.object(run, "safe_shutdown", side_effect=lambda: order.append("cleanup") or True), \
                        patch.object(run, "finalize", side_effect=lambda success, *, shutdown_attempted: order.append(("finalize", success, shutdown_attempted)) or True), \
                        patch.object(gate, "choose_output", return_value=output), \
                        patch.object(gate, "prepare_fixture_acceptance", return_value=run), \
                        patch.object(gate, "Fixture", return_value=nullcontext(fixture)), \
                        patch.object(gate, "write_json", side_effect=write), \
                        patch.dict(os.environ, {"LC_ALL": "fixture-parent-locale"}), \
                        patch.object(sys, "argv", ["acceptance_ccr_fixture.py"]):
                    with self.assertRaises(RuntimeError) as caught:
                        gate.main()
                    self.assertEqual(os.environ["LC_ALL"], "fixture-parent-locale")
                self.assertIs(caught.exception, primary)
                self.assertEqual(order, ["snapshot", "cleanup", ("finalize", False, True)])
                self.assertEqual(environment, {"LC_ALL": "de_DE", "KNOWN": "fixture"})
                if fault == "write":
                    self.assertEqual(primary.__notes__, ["host_cpu_snapshot_receipt_unavailable"])
                else:
                    receipt = json.loads((output / gate.CPU_SNAPSHOT_NAME).read_text())
                    self.assertEqual(receipt["status"], "observed" if fault == "none" else "unavailable")
                    self.assertEqual(receipt["method"], "native_ps")
                    self.assertEqual(receipt["argv"], ["ps", "-A", "-o", "pcpu=,comm="])
                    self.assertEqual(receipt["timing"], "after_resource_refusal")
                    self.assertEqual(receipt["basis"], "native_platform_historical_percent")
                    self.assertEqual(receipt["origin"], "driver_failure_observed")
                    self.assertGreaterEqual(receipt["capture_started_offset_ms"], 0)
                    if fault in ("parse", "none"):
                        self.assertGreaterEqual(receipt["capture_completed_offset_ms"], receipt["capture_started_offset_ms"])
                    else:
                        self.assertNotIn("capture_completed_offset_ms", receipt)
                    self.assertFalse(receipt["admission_intervals_matched"])
                    self.assertFalse(receipt["causal_attribution"])
                    self.assertNotIn("canary", json.dumps(receipt))
                    self.assertNotIn("/", json.dumps(receipt))

    def test_interrupts_never_launch_snapshot_and_keep_cleanup(self):
        for primary in (KeyboardInterrupt(), SystemExit(7)):
            with self.subTest(kind=type(primary).__name__), tempfile.TemporaryDirectory(prefix="ccr-cpu-interrupt-") as temporary:
                output = Path(temporary)
                run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
                fixture = SimpleNamespace(server=SimpleNamespace(server_port=43210))

                def exercise(_fixture):
                    raise primary

                with patch.multiple(run, create=True, output=output, provider_executable="/ccr",
                                    direct=lambda *_args, **_kwargs: None, exercise=exercise), \
                        patch.object(run, "capture_cpu_snapshot", side_effect=AssertionError("unexpected snapshot")) as snapshot, \
                        patch.object(run, "safe_shutdown", return_value=True) as shutdown, \
                        patch.object(run, "finalize", return_value=True) as finalize, \
                        patch.object(gate, "choose_output", return_value=output), \
                        patch.object(gate, "prepare_fixture_acceptance", return_value=run), \
                        patch.object(gate, "Fixture", return_value=nullcontext(fixture)), \
                        patch.object(sys, "argv", ["acceptance_ccr_fixture.py"]):
                    with self.assertRaises(type(primary)) as caught:
                        gate.main()
                self.assertIs(caught.exception, primary)
                snapshot.assert_not_called()
                shutdown.assert_called_once_with()
                finalize.assert_called_once_with(False, shutdown_attempted=True)


class ProviderInstallationWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="ccr-provider-workflow-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repository = Path(__file__).resolve().parents[1]
        workflow = (self.repository / ".github/workflows/ccr-integration.yml").read_text()
        step = workflow.split("      - name: Install locked Claude Code and test supervisor\n", 1)[1]
        body = step.split("      - name:", 1)[0].split("        run: |\n", 1)[1]
        self.script = "\n".join(line[10:] for line in body.splitlines())
        self.prefix = self.root / "runner temp/ccr-provider"
        self.record = self.root / "npm-arguments"
        self.events = self.root / "events"
        self.github_path = self.root / "github-path"
        self.environment = {"PATH": str(self.root / "stubs") + os.pathsep + os.environ["PATH"],
                            "RUNNER_TEMP": str(self.prefix.parent), "GITHUB_PATH": str(self.github_path),
                            "STUB_NPM_RECORD": str(self.record), "STUB_EVENTS": str(self.events)}
        source = self.root / "tools/ccr-provider"
        source.mkdir(parents=True)
        for name in ("package.json", "package-lock.json"):
            (source / name).write_bytes((self.repository / "tools/ccr-provider" / name).read_bytes())
        self.write_executable("scripts/install-test-supervisor.sh", '#!/bin/sh\necho supervisor >> "$STUB_EVENTS"\n')
        self.write_executable("stubs/npm", '''#!/bin/bash
set -euo pipefail
printf '%s\\n' "$@" > "$STUB_NPM_RECORD"
test "$1" = ci
test "$2" = --prefix
prefix="$3"
test -f "$prefix/package.json"
test -f "$prefix/package-lock.json"
if [ "${STUB_INSTALL_FAILURE:-}" = 1 ]; then exit 23; fi
mkdir -p "$prefix/node_modules/.bin"
printf '%s\\n' '#!/bin/sh' 'test ! -e "$GITHUB_PATH" || exit 99' \
    'echo claude >> "$STUB_EVENTS"' \
    'if [ "${STUB_NATIVE_FAILURE:-}" = 1 ]; then exit 24; fi' \
    'printf "%s\\n" "${STUB_CLAUDE_VERSION:-2.1.318} (Claude Code)"' > "$prefix/node_modules/.bin/claude"
chmod +x "$prefix/node_modules/.bin/claude"
''')

    def write_executable(self, relative, content):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
        path.chmod(0o755)

    def run_install(self):
        return subprocess.run(["bash", "-euo", "pipefail", "-c", self.script], cwd=self.root,
                              env=self.environment, capture_output=True, text=True, check=False, timeout=30)

    def test_install_uses_copied_lock_required_scripts_and_isolated_path(self):
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.record.read_text().splitlines(), [
            "ci", "--prefix", str(self.prefix), "--include=optional", "--ignore-scripts=false", "--no-audit", "--no-fund"])
        for name in ("package.json", "package-lock.json"):
            self.assertEqual((self.prefix / name).read_bytes(),
                             (self.repository / "tools/ccr-provider" / name).read_bytes())
        self.assertEqual(self.github_path.read_text().splitlines(), [str(self.prefix / "node_modules/.bin")])
        self.assertEqual(self.events.read_text().splitlines(), ["claude", "supervisor"])

    def test_install_failure_does_not_expose_provider_or_run_supervisor_install(self):
        self.environment["STUB_INSTALL_FAILURE"] = "1"
        result = self.run_install()
        self.assertEqual(result.returncode, 23, result.stderr)
        self.assertFalse(self.github_path.exists())
        self.assertFalse(self.events.exists())

    def test_wrong_native_version_is_rejected_before_exposing_provider(self):
        self.environment["STUB_CLAUDE_VERSION"] = "2.1.317"
        result = self.run_install()
        self.assertEqual(result.returncode, 1)
        self.assertIn("Expected Claude Code 2.1.318; received 2.1.317", result.stderr)
        self.assertFalse(self.github_path.exists())
        self.assertEqual(self.events.read_text().splitlines(), ["claude"])

    def test_native_check_failure_does_not_expose_provider_or_run_supervisor_install(self):
        self.environment["STUB_NATIVE_FAILURE"] = "1"
        result = self.run_install()
        self.assertEqual(result.returncode, 24, result.stderr)
        self.assertFalse(self.github_path.exists())
        self.assertEqual(self.events.read_text().splitlines(), ["claude"])


if __name__ == "__main__":
    unittest.main()
