"""Native provider budgets stay consistent across dispatch, persisted requests, waits and receipts."""

from argparse import Namespace
import hashlib
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from scripts.acceptance.native.tests.support import fixture_record
from scripts.acceptance.native.tests.support import task_owner
from scripts.acceptance.native import workflow as gate
from scripts.acceptance.tasks.request_intent import TaskRequest


# Delegate persists an omitted Codex review budget as its public one-hour
# default (internal/config/policy/policy.go ParseBudgetForProvider).
REVIEW_DEFAULT_BUDGET = "1h0m0s"
REVIEW_DEFAULT_BUDGET_NANOS = 3_600_000_000_000
ORDINARY_BUDGET = "5m0s"
ORDINARY_BUDGET_NANOS = 300_000_000_000
NATIVE_SUITE_SECONDS = 4 * 60 * 60


class NativeCaseBudgetContractTests(unittest.TestCase):
    """Dispatch, the persisted request, the outer wait, and the receipt share one case budget."""

    def _operation(self, provider: str, root: Path, mode: str = "read-only") -> gate.NativeAcceptance:
        operation = object.__new__(gate.NativeAcceptance)
        operation.delegate = Path("/delegate")
        operation.state = root / "state"
        operation.config = Path("/pueue.yml")
        operation.runner = Path("/runner")
        operation.workspace = root / "workspace"
        operation.workspace.mkdir()
        operation.provider = provider
        operation.mode = mode
        operation.scenario = "read-only"
        operation.root_id = "b" * 32
        self.enterContext(patch.object(operation, "ops", SimpleNamespace(managed_runner=True), create=True))
        operation.args = Namespace(model=None, effort=None)
        return operation

    def _persist_request(self, operation, budget: str, budget_nanos: int) -> str:
        task_id = "a" * 32
        task = operation.state / "tasks" / task_id
        task.mkdir(parents=True)
        brief = b"review\n"
        (task / "brief.md").write_bytes(brief)
        gate.write_json(task / "task.json", {
            "root_id": operation.root_id, "task_id": task_id, "provider": operation.provider,
            "mode": operation.mode, "canonical_cwd": str(operation.workspace),
            "requested_config": {"permission": operation.mode, "budget": budget},
            "budget_nanos": budget_nanos, "brief_length": len(brief),
            "brief_sha256": hashlib.sha256(brief).hexdigest(),
        })
        return task_id

    def _admission(self, operation, brief: Path, **options) -> tuple[list[object], TaskRequest]:
        """Capture the argv and retained request one public dispatch hands to admission."""
        captured: list[tuple[list[object], TaskRequest]] = []

        def admit(_name: str, _task_id: str, argv: list[object], *, request: TaskRequest, environment):
            captured.append((argv, request))
            raise RuntimeError("admission captured")

        self.enterContext(patch.object(operation, "ops", SimpleNamespace(managed_runner=True, dispatch=admit), create=True))
        try:
            operation.dispatch("fresh", brief, **options)
        except RuntimeError as error:
            if str(error) != "admission captured":
                raise
        if not captured:
            self.fail("dispatch did not reach the admission boundary")
        return captured[0]

    def _assert_request_budget_accepted(self, operation, task_id: str, **expected) -> None:
        # The request check precedes provider metadata; reaching the absent
        # meta.json proves the persisted budget satisfied the case contract.
        with self.assertRaises(FileNotFoundError):
            operation._request_binding(task_id, **expected)

    def _wait_timeout(self, provider: str, root: Path) -> float:
        owner = task_owner(self,
            provider=provider,
            processes=gate.Processes(root / "processes", {"PATH": "/usr/bin:/bin"}),
            delegate=Path("/delegate"), runner=Path("/runner"), pueue=Path("/pueue"),
            base=root, state=root / "state", output=root / "evidence")
        ops = gate.NativeAcceptanceTaskOps(owner)
        ops.root_id = "b" * 32
        observed: dict[str, float] = {}

        def stop(*_args, timeout, **_kwargs):
            observed["timeout"] = timeout
            raise RuntimeError("observed")

        with patch("scripts.acceptance.tasks.operations.wait_runner_terminal", stop), \
                self.assertRaisesRegex(RuntimeError, "observed"):
            ops.wait_task("fresh", "a" * 32)
        return observed["timeout"]

    def test_codex_review_dispatch_omits_budget_and_binds_the_public_default(self):
        with tempfile.TemporaryDirectory(prefix="native-review-budget-") as directory:
            root = Path(directory)
            operation = self._operation("codex:review", root)
            brief = root / "brief.md"
            brief.write_text("review\n")
            argv, request = self._admission(operation, brief)

        self.assertNotIn("--budget", argv)
        self.assertNotIn("--model", argv)
        self.assertNotIn("--effort", argv)
        self.assertEqual(argv[argv.index("--permission") + 1], "read-only")
        self.assertEqual(argv[argv.index("--review-target") + 1], "uncommitted")
        self.assertEqual(request.budget, REVIEW_DEFAULT_BUDGET)

    def test_ordinary_profiles_keep_the_finite_five_minute_and_timeout_budgets(self):
        for provider in sorted(set(gate.PROFILES) - {"codex:review"}):
            with self.subTest(provider=provider), tempfile.TemporaryDirectory(
                    prefix="native-ordinary-budget-") as directory:
                root = Path(directory)
                operation = self._operation(provider, root)
                brief = root / "brief.md"
                brief.write_text("fresh\n")
                argv, request = self._admission(operation, brief)
                timeout_argv, timeout_request = self._admission(operation, brief, budget="3m")
                self.assertEqual(argv[argv.index("--budget") + 1], "5m")
                self.assertEqual(timeout_argv[timeout_argv.index("--budget") + 1], "3m")
                self.assertEqual((request.budget, timeout_request.budget), ("5m", "3m"))

    def test_persisted_request_budget_matches_the_provider_case(self):
        cases = (
            ("codex:review", REVIEW_DEFAULT_BUDGET, REVIEW_DEFAULT_BUDGET_NANOS,
             ORDINARY_BUDGET, ORDINARY_BUDGET_NANOS),
            ("codex:exec", ORDINARY_BUDGET, ORDINARY_BUDGET_NANOS,
             REVIEW_DEFAULT_BUDGET, REVIEW_DEFAULT_BUDGET_NANOS),
        )
        for provider, budget, nanos, foreign, foreign_nanos in cases:
            with self.subTest(provider=provider):
                with tempfile.TemporaryDirectory(prefix="native-persisted-budget-") as directory:
                    operation = self._operation(provider, Path(directory))
                    task_id = self._persist_request(operation, budget, nanos)
                    self._assert_request_budget_accepted(operation, task_id)
                with tempfile.TemporaryDirectory(prefix="native-persisted-budget-") as directory:
                    operation = self._operation(provider, Path(directory))
                    task_id = self._persist_request(operation, foreign, foreign_nanos)
                    with self.assertRaisesRegex(gate.AcceptanceFailure, "request binding is invalid"):
                        operation._request_binding(task_id)

    def test_timeout_source_budget_override_stays_three_minutes(self):
        with tempfile.TemporaryDirectory(prefix="native-timeout-budget-") as directory:
            operation = self._operation("codex:exec", Path(directory), mode="workspace-write")
            task_id = self._persist_request(operation, "3m0s", 180_000_000_000)
            self._assert_request_budget_accepted(
                operation, task_id, budget="3m0s", budget_nanos=180_000_000_000)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "request binding is invalid"):
                operation._request_binding(task_id)
            with self.assertRaisesRegex(gate.AcceptanceFailure, "both its text and nanoseconds"):
                operation._request_binding(task_id, budget="3m0s")

    def test_outer_runner_wait_outlives_the_persisted_budget_within_the_native_suite(self):
        for provider, budget_seconds in (("codex:review", REVIEW_DEFAULT_BUDGET_NANOS // 10 ** 9),
                                         ("codex:exec", ORDINARY_BUDGET_NANOS // 10 ** 9),
                                         ("claude:print", ORDINARY_BUDGET_NANOS // 10 ** 9)):
            with self.subTest(provider=provider), tempfile.TemporaryDirectory(
                    prefix="native-outer-wait-") as directory:
                timeout = self._wait_timeout(provider, Path(directory))
                self.assertGreater(timeout, budget_seconds)
                self.assertLessEqual(timeout, budget_seconds + 60)
                self.assertLess(timeout, NATIVE_SUITE_SECONDS)

    def test_codex_review_receipt_records_the_omitted_request_and_persisted_default(self):
        with tempfile.TemporaryDirectory(prefix="native-review-receipt-") as directory:
            operation = self._operation("codex:review", Path(directory))
            task_id = "a" * 32
            dispatched: list[tuple[tuple[object, ...], dict[str, object]]] = []
            operation.tasks = {"fresh": task_id}
            operation.records = {"fresh-collect": {}}
            operation.model_discovery_summary = None
            operation.brief = Mock(side_effect=lambda name: Path("/brief"))
            operation.dispatch = Mock(side_effect=lambda *args, **kwargs: (dispatched.append((args, kwargs)), task_id)[1])
            for name in ("wait_done", "collect", "task_binding", "replay", "shutdown"):
                setattr(operation, name, lambda *_args, **_kwargs: None)
            operation.session_ref = Mock(return_value="session")
            operation.native_mode_receipt = Mock(return_value={})
            operation.provider_runtime_receipt = Mock(return_value={})
            operation.lifecycle_receipt = Mock(return_value={})
            self.assertEqual(operation._run_review(), 0)

        self.assertEqual(dispatched, [(("fresh", Path("/brief")), {})])
        receipt = fixture_record(operation.success_receipt)
        self.assertIsNone(receipt["task_budget"])
        self.assertEqual(receipt["canonical_task_budget"], REVIEW_DEFAULT_BUDGET)
        self.assertEqual((receipt["requested_model"], receipt["requested_effort"]), (None, None))
