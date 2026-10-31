"""Cleanup ownership checks for the CCR fixture gate."""

import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import acceptance_ccr_fixture as gate
from acceptance_supervisor_common import DirectoryOwner
from scripts.acceptance_tests.resources import track_directory_pins
from scripts.delegate_acceptance.native import acceptance_lifecycle as lifecycle_impl


class FixtureCleanupTests(unittest.TestCase):
    @staticmethod
    def fake_run(shutdown_result=True, finalize_result=True, finalize_error=None,
                 output=None):
        calls = []

        class Run:
            def __init__(self):
                self.output = output

            def safe_shutdown(self):
                calls.append("shutdown")
                return shutdown_result

            def finalize(self, success):
                calls.append(("finalize", success))
                if finalize_error is not None:
                    raise finalize_error
                return finalize_result

        return Run(), calls

    def test_success_finalizes_once_after_proven_shutdown(self):
        run, calls = self.fake_run()

        gate.cleanup_fixture(run, True)

        self.assertEqual(calls, ["shutdown", ("finalize", True)])

    def test_failure_still_finalizes_once_and_cleanup_failure_is_fatal(self):
        run, calls = self.fake_run(shutdown_result=False)

        with self.assertRaisesRegex(RuntimeError, "fixture cleanup failed"):
            gate.cleanup_fixture(run, False)

        self.assertEqual(calls, ["shutdown", ("finalize", False)])

    def test_finalize_failure_is_fatal(self):
        run, calls = self.fake_run(finalize_result=False)

        with self.assertRaisesRegex(RuntimeError, "fixture cleanup failed"):
            gate.cleanup_fixture(run, True)

        self.assertEqual(calls, ["shutdown", ("finalize", True)])

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

                self.assertEqual(calls, ["shutdown", ("finalize", True)])
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
        run.processes = SimpleNamespace(capture_root=raw)
        run.closed, run.success_candidate = True, None
        run.args, run.mode = SimpleNamespace(model="ok"), "read-only"
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
            rows = {str(number): {"status": {"Done": {}}} for number in run.numeric_ids.values()}
            return SimpleNamespace(json=lambda: {"tasks": rows})

        run.direct = lambda *_args, **_kwargs: None
        run.setup = lambda: None
        run.ops = SimpleNamespace(client=client)
        run.dispatch, run.command = dispatch, command
        run.wait_done = lambda *_args: None
        run.session_ref = lambda task_id: sessions[task_id]

    def main(self, run: gate.FixtureAcceptance, *patches):
        argv = ["acceptance_ccr_fixture.py", "--output", str(self.output)]
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
        self.assertTrue(run.success_candidate.confirmed)
        self.assertEqual(run.success_candidate.sha256, hashlib.sha256(data).hexdigest())
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
    def test_ci_retains_only_existing_public_control_records(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/ccr-integration.yml"
        upload = workflow.read_text().split("      - name: Retain sanitized result\n", 1)[1]
        section = upload.split("          path:", 1)[1].split("          if-no-files-found:", 1)[0]
        paths = {line.strip() for line in section.splitlines() if line.strip() not in ("", "|")}
        root = "~/ccr-integration-evidence"
        self.assertEqual(paths, {f"{root}/{name}" for name in (
            "success.json", "*-dispatch-attempt.json", "*-continue-attempt.json",
            "failure-diagnostics.json", "failure-cleanup.json", "failure-cleanup-expired.json")})
        self.assertIn("if: always()", upload)


if __name__ == "__main__":
    unittest.main()
