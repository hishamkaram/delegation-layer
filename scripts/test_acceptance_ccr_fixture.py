"""Cleanup ownership checks for the CCR fixture gate."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import acceptance_ccr_fixture as gate
from acceptance_supervisor_common import DirectoryOwner
from scripts.acceptance_tests.resources import track_directory_pins
from scripts.delegate_acceptance.native import acceptance_lifecycle as lifecycle_impl
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.hermetic.contract import status_document, status_job


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

    def test_finalize_forwards_blocked_state_to_native_owner(self):
        run = gate.FixtureAcceptance.__new__(gate.FixtureAcceptance)
        run.fixture_directories = ()
        with patch.object(gate.NativeAcceptance, "finalize", return_value=True) as finalize:
            self.assertTrue(run.finalize(False, blocked=True))
        finalize.assert_called_once_with(False, blocked=True)

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
            ops=SimpleNamespace(client=client), dispatch=dispatch, command=command,
            wait_done=lambda *_args: None, session_ref=lambda task_id: sessions[task_id])
        patched_cli.start()
        self.addCleanup(patched_cli.stop)

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
        self.assertEqual(paths, {f"{root}/{name}" for name in (
            "success.json", "*-dispatch-attempt.json", "*-continue-attempt.json",
            "failure-diagnostics.json", "failure-cleanup.json", "failure-cleanup-expired.json")})
        self.assertIn("if: always()", upload)


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
