"""Typed block reasons at the native acceptance detection sites and receipts.

Every case drives the production call site that chooses the reason, with the
finite fixtures the neighbouring native cohorts already use. English wording
never selects a neutral reason: only structured facts do.
"""

from argparse import Namespace
from collections.abc import Mapping
from typing import ClassVar

import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from scripts import acceptance_native as entry
from scripts.acceptance_tests.native.support import invoke_with_invalid_arguments
from scripts.delegate_acceptance.native import (
    acceptance as gate,
    acceptance_discovery as discovery_impl,
    timeouts,
)
from scripts.delegate_acceptance.native.errors import BlockedFailure
from scripts.delegate_acceptance.native_evidence.contract import BlockReason
from scripts.delegate_acceptance.tasks.operations import AdmissionFailure


AUTH_BLOCK_RESPONSE = {
    "status": "blocked",
    "capability": {"live_acceptance": {
        "status": "blocked", "authentication": "blocked",
        "reason_code": "authentication_unavailable",
    }},
}
ENGLISH_AUTH_TEXT = "provider authentication unavailable: please sign in again"
STRUCTURED_AUTH_DIAGNOSTIC: dict[str, object] = {
    "code": "authentication-unavailable", "event": 1,
    "message": "Provider authentication was unavailable.",
}


def _write_executable(path: Path) -> Path:
    path.write_text("#!/bin/sh\nexit 0\n")
    path.chmod(0o700)
    return path


class BlockedFailureContractTests(unittest.TestCase):
    def test_reason_defaults_to_unclassified_and_the_message_is_preserved(self):
        error = BlockedFailure("pueue is not discoverable")
        self.assertIs(error.reason, BlockReason.UNCLASSIFIED)
        self.assertEqual(str(error), "pueue is not discoverable")
        self.assertIsInstance(error, gate.AcceptanceFailure)

    def test_every_canonical_reason_is_carried_exactly(self):
        for reason in BlockReason:
            with self.subTest(reason=reason):
                self.assertIs(BlockedFailure("blocked", reason=reason).reason, reason)

    def test_only_a_block_reason_member_is_accepted(self):
        for candidate in ("auth-unavailable", None, 1, BlockReason.AUTH_UNAVAILABLE.value,
                          SimpleNamespace(value="auth-unavailable")):
            with self.subTest(candidate=candidate):
                with self.assertRaises(TypeError):
                    invoke_with_invalid_arguments(BlockedFailure, ("blocked",), {"reason": candidate})

    def test_the_reason_is_keyword_only(self):
        with self.assertRaises(TypeError):
            invoke_with_invalid_arguments(BlockedFailure, ("blocked", BlockReason.AUTH_UNAVAILABLE), {})


class AuthenticationStatusSiteTests(unittest.TestCase):
    def _operation(self, directory: str, stdout: str, exit_code: int):
        process_directory = Path(directory)
        (process_directory / "stdout").write_text(stdout, encoding="utf-8")
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider = "claude:print"
        operation.profile = gate.PROFILES[operation.provider]
        operation.provider_executable = Path("/usr/bin/claude")
        operation.direct = Mock(side_effect=lambda name, argv, expected, timeout: SimpleNamespace(
            directory=process_directory, result={"exit_code": exit_code}))
        return operation

    def test_logged_out_status_is_auth_unavailable(self):
        with tempfile.TemporaryDirectory(prefix="block-reason-auth-status-") as directory:
            operation = self._operation(directory, '{"loggedIn":false}\n', 1)
            with self.assertRaises(BlockedFailure) as raised:
                operation.require_authentication_prerequisite()
            self.assertIs(raised.exception.reason, BlockReason.AUTH_UNAVAILABLE)

    def test_english_auth_text_beside_a_logged_in_status_is_not_a_block(self):
        with tempfile.TemporaryDirectory(prefix="block-reason-auth-text-") as directory:
            operation = self._operation(
                directory, json.dumps({"loggedIn": True, "note": ENGLISH_AUTH_TEXT}) + "\n", 0)
            operation.require_authentication_prerequisite()

    def test_status_without_a_logged_in_boolean_is_a_failure_not_a_block(self):
        for stdout in (json.dumps({"message": ENGLISH_AUTH_TEXT}), '{"loggedIn":"false"}',
                       '{"loggedIn":0}', '{"loggedIn":null}'):
            with self.subTest(stdout=stdout), tempfile.TemporaryDirectory(
                    prefix="block-reason-auth-malformed-") as directory:
                operation = self._operation(directory, stdout + "\n", 1)
                with self.assertRaises(gate.AcceptanceFailure) as raised:
                    operation.require_authentication_prerequisite()
                self.assertNotIsInstance(raised.exception, BlockedFailure)


class ModelDiscoverySiteTests(unittest.TestCase):
    def _prerequisite(self, response: dict[str, object]):
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider = "pi:json"
        operation.args = Namespace(model=None)
        operation.discover_models = Mock()
        operation.model_discovery = response
        operation.require_default_model_prerequisite()

    def test_typed_authentication_block_is_auth_unavailable(self):
        with self.assertRaises(BlockedFailure) as raised:
            self._prerequisite({"status": "blocked", "reason_code": "authentication_unavailable"})
        self.assertIs(raised.exception.reason, BlockReason.AUTH_UNAVAILABLE)

    def test_an_empty_local_catalog_is_not_credential_unavailability(self):
        with self.assertRaises(BlockedFailure) as raised:
            self._prerequisite({"status": "unavailable", "reason_code": "models_unavailable",
                                "models": []})
        self.assertIs(raised.exception.reason, BlockReason.UNCLASSIFIED)

    def test_other_blocked_reasons_stay_failures(self):
        with self.assertRaises(gate.AcceptanceFailure) as raised:
            self._prerequisite({"status": "blocked", "reason_code": "supervisor_upgrade_required"})
        self.assertNotIsInstance(raised.exception, BlockedFailure)

    def test_classifier_reads_only_the_typed_status_and_reason_code(self):
        neutral = {"status": "blocked", "reason_code": "authentication_unavailable"}
        self.assertIs(discovery_impl.model_discovery_block_reason(neutral), BlockReason.AUTH_UNAVAILABLE)
        for response in (
                {"status": "unavailable", "reason_code": "authentication_unavailable"},
                {"status": "failed", "reason_code": "authentication_unavailable"},
                {"status": "blocked", "reason_code": "Authentication_Unavailable"},
                {"status": "blocked", "reason_code": "authentication_unavailable "},
                {"status": "blocked", "reason_code": ENGLISH_AUTH_TEXT},
                {"status": "blocked", "error": ENGLISH_AUTH_TEXT},
                {"status": "unavailable", "reason_code": "models_unavailable"},
                {}, None, ENGLISH_AUTH_TEXT, ["blocked", "authentication_unavailable"]):
            with self.subTest(response=response):
                self.assertIs(discovery_impl.model_discovery_block_reason(response),
                              BlockReason.UNCLASSIFIED)


class AdmissionAndCollectionSiteTests(unittest.TestCase):
    def _dispatch_operation(self, response: Mapping[str, object] | None):
        def refuse(*_arguments, **_request):
            if response is None:
                raise gate.AcceptanceFailure("dispatch refused")
            raise AdmissionFailure("dispatch refused", dict(response))

        # Dispatch binds its request inputs before launch, as the real constructor provides them.
        temporary = tempfile.TemporaryDirectory(prefix="native-block-dispatch-")
        self.addCleanup(temporary.cleanup)
        self.brief = Path(temporary.name) / "brief"
        self.brief.write_bytes(b"brief\n")
        operation = object.__new__(gate.NativeAcceptance)
        operation.provider = "codex:exec"
        operation.mode, operation.scenario = "read-only", "read-only"
        operation.workspace = Path(temporary.name)
        operation.args = Namespace(model=None, effort=None)
        operation.root_id = None
        operation.dispatch_argv = Mock(return_value=["delegate"])
        operation.task_command_prefix = Mock(return_value=["delegate"])
        operation._sync_ops_owned_tasks = Mock()
        self.enterContext(patch.object(operation, "ops", SimpleNamespace(
            root_id=None, dispatch=refuse, continue_task=refuse), create=True))
        return operation

    def test_structured_admission_block_is_auth_unavailable_for_dispatch_and_continue(self):
        operation = self._dispatch_operation(AUTH_BLOCK_RESPONSE)
        calls = (lambda: operation.dispatch("fresh", self.brief),
                 lambda: operation.continue_public("resume", self.brief, "a" * 32))
        for call in calls:
            with self.subTest(call=call), self.assertRaises(BlockedFailure) as raised:
                call()
            self.assertIs(raised.exception.reason, BlockReason.AUTH_UNAVAILABLE)
            self.assertIsInstance(raised.exception.__cause__, gate.AcceptanceFailure)
            self.assertEqual(str(raised.exception.__cause__), "dispatch refused")

    def test_english_admission_error_text_is_not_a_block(self):
        for response in ({"status": "refused", "error": ENGLISH_AUTH_TEXT}, {}, None):
            operation = self._dispatch_operation(response)
            for call in (lambda: operation.dispatch("fresh", self.brief),
                         lambda: operation.continue_public(
                             "resume", self.brief, "a" * 32)):
                with self.subTest(response=response), self.assertRaises(
                        gate.AcceptanceFailure) as raised:
                    call()
                self.assertNotIsInstance(raised.exception, BlockedFailure)
                self.assertEqual(str(raised.exception), "dispatch refused")

    def _wait_operation(self, state: Path, outcome: Mapping[str, object] | None):
        task = "a" * 32
        task_directory = state / "tasks" / task
        task_directory.mkdir(parents=True)
        if outcome is not None:
            gate.write_json(task_directory / "outcome.json", outcome)

        def failed(*_arguments):
            raise gate.AcceptanceFailure("supervisor result 'Failed'")

        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = "b" * 32
        operation.state = state
        operation.provider = "ccr:print"
        self.enterContext(patch.object(operation, "ops", SimpleNamespace(
            root_id=None, wait_task=failed,
            status=Mock(return_value={"admission": "admitted", "liveness": "stopped"})), create=True))
        return operation, task

    def test_runner_failure_with_a_structured_diagnostic_is_auth_unavailable(self):
        with tempfile.TemporaryDirectory(prefix="block-reason-wait-") as directory:
            operation, task = self._wait_operation(Path(directory), {
                "verdict": "rejected", "diagnostics": [STRUCTURED_AUTH_DIAGNOSTIC]})
            with self.assertRaises(BlockedFailure) as raised:
                operation.wait_done("fresh", task)
            self.assertIs(raised.exception.reason, BlockReason.AUTH_UNAVAILABLE)

    def test_runner_failure_with_english_diagnostic_text_stays_a_failure(self):
        english: dict[str, object] = {"code": "provider-failed", "event": 1, "message": ENGLISH_AUTH_TEXT}
        for outcome in (None, {"verdict": "rejected", "diagnostics": [english]}):
            with self.subTest(outcome=outcome), tempfile.TemporaryDirectory(
                    prefix="block-reason-wait-english-") as directory:
                operation, task = self._wait_operation(Path(directory), outcome)
                with self.assertRaises(gate.AcceptanceFailure) as raised:
                    operation.wait_done("fresh", task)
                self.assertNotIsInstance(raised.exception, BlockedFailure)

    def _collect_operation(self, root: Path, diagnostics: list[dict[str, object]]):
        task, root_id = "a" * 32, "b" * 32
        state = root / "state"
        task_directory = state / "tasks" / task
        task_directory.mkdir(parents=True)
        refusal = b"REFUSAL_PAYLOAD"
        payload = {"basename": "publish.reject", "length": len(refusal),
                   "sha256": hashlib.sha256(refusal).hexdigest()}
        evidence = "e" * 64
        outcome = {"verdict": "rejected", "payload": payload, "evidence_sha256": evidence,
                   "diagnostics": diagnostics}
        gate.write_json(task_directory / "outcome.json", outcome)
        (task_directory / "publish.reject").write_bytes(refusal)
        gate.write_json(task_directory / "provider.exit", {
            "root_id": root_id, "task_id": task, "invocation_state": "started",
            "exit_code": 0, "error": "", "raw_manifest": [], "manifest_sha256": evidence})
        response = {"task_id": task, "publication": "rejected", "outcome": outcome,
                    "payload": payload, "evidence_sha256": evidence}
        operation = object.__new__(gate.NativeAcceptance)
        operation.root_id = root_id
        operation.state = state
        operation.provider = "pi:json"
        self.enterContext(patch.object(operation, "ops", SimpleNamespace(
            root_id=None, collect=lambda *_arguments, **_keywords: response), create=True))
        return operation, task

    def test_rejected_collection_with_a_structured_diagnostic_is_auth_unavailable(self):
        with tempfile.TemporaryDirectory(prefix="block-reason-collect-") as directory:
            operation, task = self._collect_operation(Path(directory), [STRUCTURED_AUTH_DIAGNOSTIC])
            with self.assertRaises(BlockedFailure) as raised:
                operation.collect("fresh-collect", task)
            self.assertIs(raised.exception.reason, BlockReason.AUTH_UNAVAILABLE)

    def test_rejected_collection_with_english_text_stays_a_failure(self):
        english: dict[str, object] = {"code": "provider-failed", "event": 1, "message": ENGLISH_AUTH_TEXT}
        with tempfile.TemporaryDirectory(prefix="block-reason-collect-english-") as directory:
            operation, task = self._collect_operation(Path(directory), [english])
            with self.assertRaises(gate.AcceptanceFailure) as raised:
                operation.collect("fresh-collect", task)
            self.assertNotIsInstance(raised.exception, BlockedFailure)


class ExecutableSiteTests(unittest.TestCase):
    """The NativeAcceptance constructor resolves each executable before any directory exists."""

    def _construct(self, root: Path, *, provider: str = "pi:json", omit: str | None = None,
                   provider_executable: str | None = None, **extra: str):
        tools = root / "tools"
        tools.mkdir(exist_ok=True)
        output = root / "output"
        output.mkdir(exist_ok=True)
        arguments = ["--provider", provider, "--permission", "read-only", "--output", str(output)]
        selected = {name: str(tools / name) for name in
                    ("delegate", "delegate-run", "pueue", "pueued", "git",
                     gate.PROFILES[provider]["executable"])}
        for name, path in selected.items():
            if name != omit:
                _write_executable(Path(path))
        for flag, name in (("--delegate", "delegate"), ("--runner", "delegate-run"),
                           ("--pueue", "pueue"), ("--pueued", "pueued")):
            arguments.extend([flag, selected[name]])
        if provider_executable is not None:
            arguments.extend(["--provider-executable", provider_executable])
        args = entry.build_parser().parse_args(arguments)
        for key, value in extra.items():
            setattr(args, key, value)
        # The constructor resolves bare names through both the process and the native PATH.
        with patch.dict(os.environ, {"PATH": str(tools)}), \
                patch.object(gate, "native_environment", return_value={"PATH": str(tools)}), \
                patch.object(gate, "reject_tmp"):
            return gate.NativeAcceptance(args)

    def _reason(self, **keywords) -> BlockReason:
        with tempfile.TemporaryDirectory(prefix="block-reason-executable-") as directory:
            return self._reason_in(Path(directory).resolve(), **keywords)

    def test_missing_provider_executable_is_executable_unavailable(self):
        provider = gate.PROFILES["pi:json"]["executable"]
        self.assertIs(self._reason(omit=provider), BlockReason.EXECUTABLE_UNAVAILABLE)

    def test_explicit_missing_or_non_executable_provider_is_executable_unavailable(self):
        with tempfile.TemporaryDirectory(prefix="block-reason-explicit-") as directory:
            root = Path(directory).resolve()
            plain = root / "plain-file"
            plain.write_text("not executable\n")
            plain.chmod(0o600)
            for candidate in (root / "absent", plain):
                with self.subTest(candidate=candidate.name):
                    self.assertIs(self._reason_in(root, provider_executable=str(candidate)),
                                  BlockReason.EXECUTABLE_UNAVAILABLE)

    def test_harness_binaries_stay_unclassified(self):
        for name in ("delegate", "delegate-run", "pueue", "pueued"):
            with self.subTest(missing=name):
                self.assertIs(self._reason(omit=name), BlockReason.UNCLASSIFIED)

    def test_provider_path_shadowing_stays_unclassified(self):
        with tempfile.TemporaryDirectory(prefix="block-reason-shadow-") as directory:
            root = Path(directory).resolve()
            (root / "elsewhere").mkdir()
            shadow = _write_executable(root / "elsewhere" / gate.PROFILES["pi:json"]["executable"])
            self.assertIs(self._reason_in(root, provider_executable=str(shadow)),
                          BlockReason.UNCLASSIFIED)

    def _reason_in(self, root: Path, **keywords) -> BlockReason:
        with self.assertRaises(BlockedFailure) as raised:
            self._construct(root, **keywords)
        return raised.exception.reason

    def test_codex_git_prerequisite_stays_unclassified(self):
        real = discovery_impl.resolve_executable

        def resolve(value, default, label):
            if label == "git":
                raise BlockedFailure("git is unavailable or not executable")
            return real(value, default, label)

        with tempfile.TemporaryDirectory(prefix="block-reason-git-") as directory, \
                patch.object(gate, "resolve_executable", side_effect=resolve):
            with self.assertRaises(BlockedFailure) as raised:
                self._construct(Path(directory).resolve(), provider="codex:exec")
        self.assertIs(raised.exception.reason, BlockReason.UNCLASSIFIED)
        self.assertEqual(str(raised.exception), "Codex git prerequisite is unavailable")

    def test_non_git_base_inside_git_stays_unclassified(self):
        with tempfile.TemporaryDirectory(prefix="block-reason-non-git-") as directory:
            root = Path(directory).resolve()
            with patch.object(Path, "home", return_value=root), \
                    patch.object(gate, "has_git_ancestor", return_value=True):
                with self.assertRaises(BlockedFailure) as raised:
                    self._construct(root, provider="codex:exec", scenario="non-git")
        self.assertIs(raised.exception.reason, BlockReason.UNCLASSIFIED)

    def test_unsupported_platform_lease_stays_unclassified(self):
        with tempfile.TemporaryDirectory(prefix="block-reason-platform-") as directory, \
                patch.object(timeouts, "sys", SimpleNamespace(platform="win32")):
            with self.assertRaises(BlockedFailure) as raised:
                timeouts.TimeoutFifoLease(Path(directory) / "lease", "lease", "ready")
        self.assertIs(raised.exception.reason, BlockReason.UNCLASSIFIED)


class MainReceiptTests(unittest.TestCase):
    def _main(self, run_factory, *arguments: str):
        with tempfile.TemporaryDirectory(prefix="block-reason-main-") as directory:
            output = Path(directory) / "evidence"
            output.mkdir()
            with patch.object(entry, "choose_output", return_value=output), \
                    patch.object(entry, "NativeAcceptance", return_value=run_factory()), \
                    patch("sys.stderr"), patch("sys.stdout"):
                code = entry.main(["--provider", "pi:json", "--permission", "read-only",
                                   "--output", str(output), *arguments])
            return code, gate.read_json(output / "failure.json")

    @staticmethod
    def _run(*, result: int | None = None, error: BaseException | None = None,
             summary: object = None, discovery_exit_code: int = 2):
        class Run:
            model_discovery_summary: ClassVar[object]
            model_discovery_exit_code = discovery_exit_code

            def run(self):
                if error is not None:
                    raise error
                return result

            def finalize(self, _success, *, blocked=False):
                return True

            def publish_success(self):
                raise AssertionError("failure published success")

        if summary is not None:
            Run.model_discovery_summary = summary
        return Run

    def test_exception_path_records_every_reason_beside_the_unchanged_message(self):
        for reason in BlockReason:
            with self.subTest(reason=reason):
                code, receipt = self._main(self._run(
                    error=BlockedFailure("blocked prerequisite", reason=reason)))
                self.assertEqual(code, 2)
                self.assertEqual((receipt["status"], receipt["error"], receipt["block_reason"]),
                                 ("BLOCKED", "blocked prerequisite", reason.value))

    def test_untyped_blocked_failure_records_unclassified(self):
        code, receipt = self._main(self._run(error=BlockedFailure("pueue is not discoverable")))
        self.assertEqual((code, receipt["block_reason"]), (2, BlockReason.UNCLASSIFIED.value))

    def test_model_flag_prerequisites_record_unclassified(self):
        for provider, fragment in (("ccr:print", "configured alias"),
                                   ("opencode:run", "tool-capable configured route")):
            with self.subTest(provider=provider), tempfile.TemporaryDirectory(
                    prefix="block-reason-model-flag-") as directory:
                output = Path(directory) / "evidence"
                output.mkdir()
                with patch.object(entry, "choose_output", return_value=output), \
                        patch.object(entry, "NativeAcceptance") as acceptance, \
                        patch("sys.stderr"):
                    code = entry.main(["--provider", provider, "--permission", "workspace-write",
                                       "--output", str(output)])
                acceptance.assert_not_called()
                receipt = gate.read_json(output / "failure.json")
                self.assertEqual(code, 2)
                self.assertIn(fragment, receipt["error"])
                self.assertEqual(receipt["block_reason"], BlockReason.UNCLASSIFIED.value)

    def test_failures_never_carry_a_block_reason(self):
        for error in (gate.AcceptanceFailure("provider rejected the task"),
                      RuntimeError("unexpected harness failure"),
                      OSError("disk")):
            with self.subTest(error=type(error).__name__):
                code, receipt = self._main(self._run(error=error))
                self.assertEqual((code, receipt["status"]), (1, "failed"))
                self.assertNotIn("block_reason", receipt)

    def test_model_discovery_code_two_without_an_exception_records_the_typed_reason(self):
        cases = (
            ({"status": "blocked", "reason_code": "authentication_unavailable"}, BlockReason.AUTH_UNAVAILABLE),
            ({"status": "blocked", "reason_code": "supervisor_upgrade_required"}, BlockReason.UNCLASSIFIED),
            ({"status": "unavailable", "reason_code": "models_unavailable"}, BlockReason.UNCLASSIFIED),
            ({"status": "unavailable", "reason_code": "authentication_unavailable"}, BlockReason.UNCLASSIFIED),
            ({"status": "blocked", "reason_code": ENGLISH_AUTH_TEXT}, BlockReason.UNCLASSIFIED),
        )
        for summary, expected in cases:
            with self.subTest(summary=summary):
                code, receipt = self._main(self._run(result=2, summary=summary),
                                           "--discover-models")
                self.assertEqual((code, receipt["status"]), (2, "BLOCKED"))
                self.assertEqual(receipt["block_reason"], expected.value)
                self.assertEqual(receipt["model_discovery"]["reason_code"], summary["reason_code"])

    def test_model_discovery_code_two_without_a_summary_is_unclassified(self):
        code, receipt = self._main(self._run(result=2), "--discover-models")
        self.assertEqual((code, receipt["block_reason"]), (2, BlockReason.UNCLASSIFIED.value))

    def test_model_discovery_failure_exit_carries_no_reason(self):
        summary = {"status": "failed", "reason_code": "authentication_unavailable"}
        code, receipt = self._main(
            self._run(result=1, summary=summary, discovery_exit_code=1), "--discover-models")
        self.assertEqual((code, receipt["status"]), (1, "failed"))
        self.assertNotIn("block_reason", receipt)

    def test_ordinary_code_two_without_an_exception_is_unclassified(self):
        code, receipt = self._main(self._run(result=2))
        self.assertEqual((code, receipt["status"], receipt["block_reason"]),
                         (2, "BLOCKED", BlockReason.UNCLASSIFIED.value))

    def test_ordinary_code_one_carries_no_reason(self):
        code, receipt = self._main(self._run(result=1))
        self.assertEqual((code, receipt["status"]), (1, "failed"))
        self.assertNotIn("block_reason", receipt)

    def test_discovery_block_flows_from_the_prerequisite_site_to_the_receipt(self):
        def run_factory():
            operation = object.__new__(gate.NativeAcceptance)
            operation.provider = "pi:json"
            operation.args = Namespace(model=None)
            operation.discover_models = Mock()
            operation.model_discovery = {"status": "blocked",
                                         "reason_code": "authentication_unavailable"}
            operation.finalize = Mock(return_value=True)
            operation.run = Mock(side_effect=operation.require_default_model_prerequisite)
            return operation

        code, receipt = self._main(run_factory)
        self.assertEqual((code, receipt["block_reason"]), (2, BlockReason.AUTH_UNAVAILABLE.value))
        self.assertEqual(receipt["error"], "Pi model discovery authentication is unavailable")


if __name__ == "__main__":
    unittest.main()
