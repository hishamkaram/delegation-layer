"""Success-only shutdown proves exact managed lifetime closure from canonical records."""

import json
import shutil
from dataclasses import dataclass
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from scripts.acceptance_supervisor_common import digest
from scripts.acceptance_tests.inspection.fixtures import ROOT, TASK, InspectionFixture
from scripts.acceptance_tests.test_foundation_owners import failing_lstat
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.native import acceptance_lifecycle as lifecycle_impl
from scripts.delegate_acceptance.native_evidence.lifetime_closure import (
    INSPECTION_FACT_KEYS,
    TASK_FACT_KEYS,
    lifetime_closure_error,
)
DENIED_KEYS = frozenset({
    "argv", "env", "environment", "stdout", "stderr", "output", "provider_output",
    "failure", "failure_detail", "diagnostic_text", "receipt_path", "archive_path",
    "checksums_path", "journal_path", "executable", "provider_runtime_identities",
    "creation_identity", "command", "commands",
})


@dataclass(frozen=True)
class ClosureRoot:
    state: Path
    base: Path


def acceptance_for(fixture: InspectionFixture | ClosureRoot, tasks: dict[str, str], *,
                   managed: bool = True, models_state: Path | None = None,
                   model_task: str | None = None) -> SimpleNamespace:
    """Project the acceptance attributes the closure producer reads."""
    ops = SimpleNamespace(managed_runner=managed, dispatch_attempts=set(tasks.values()),
                          inspection_attempts=set())
    return SimpleNamespace(
        ops=ops, tasks=dict(tasks), state=fixture.state,
        models_state=models_state or fixture.base / "models-state",
        model_discovery_summary=None if models_state is None else {"status": "available"},
        model_discovery_task_id=model_task, lifetime_closure=None, closed=False)


def managed_mixed_root(check: unittest.TestCase, *, cause: str = "completed",
                       include_wait: bool = True) -> InspectionFixture:
    """Seed a CCR-shaped root: managed runtime inspection plus managed ordinary task."""
    fixture = InspectionFixture(check)
    fixture.inspection_records(managed_exit_proof=True)
    ordinary = fixture.durable_ordinary_admission(runner_ownership="managed")
    fixture.managed_ordinary_lifetime(ordinary, termination_cause=cause, include_wait=include_wait)
    return fixture


def walk_keys(value: object) -> set[str]:
    if isinstance(value, dict):
        return set(value) | set().union(*(walk_keys(item) for item in value.values()))
    return set()


class ManagedClosureProducerTests(unittest.TestCase):
    def test_complete_managed_root_records_sanitized_bound_facts(self):
        fixture = managed_mixed_root(self)
        acceptance = acceptance_for(fixture, {"fresh": TASK})
        lifecycle_impl.record_lifetime_closure(acceptance)
        proof = acceptance.lifetime_closure
        ordinary = proof["ordinary"]
        self.assertEqual((proof["kind"], ordinary["root_id"], ordinary["launch"]),
                         ("native-lifetime-closure", ROOT, "admitted"))
        task = ordinary["tasks"][TASK]
        lifetime = fixture.state / "tasks" / TASK / "lifetime"
        self.assertEqual(set(task), TASK_FACT_KEYS)
        self.assertEqual(task, {
            "cause": "completed", "scope_empty": True, "launch_count": 1,
            "wait_result": "success", "recovered": False,
            "registration_sha256": digest(lifetime / "registration.json"),
            "termination_sha256": digest(lifetime / "terminated.json"),
            "wait_sha256": digest(lifetime / "worker-wait.json"),
            "spec_sha256": digest(fixture.state / "tasks" / TASK / "task.json"),
            "meta_sha256": digest(fixture.state / "tasks" / TASK / "meta.json"),
        })
        inspection = ordinary["inspections"][TASK]
        self.assertEqual(set(inspection), INSPECTION_FACT_KEYS)
        self.assertEqual(inspection["request_sha256"],
                         digest(fixture.state / "inspections" / TASK / "request.json"))
        self.assertIsNone(proof["model_discovery"])
        self.assertFalse(walk_keys(proof) & ({"pid", "boot_id", "argv", "env"} | DENIED_KEYS))
        self.assertNotIn(str(fixture.base), json.dumps(proof))
        self.assertIsNone(lifetime_closure_error(proof, task_ids=[TASK]))

    def test_missing_wait_partial_or_unproven_closure_fails_without_a_proof(self):
        cases = {
            "missing-wait": (lambda: managed_mixed_root(self, include_wait=False), "Wait is missing"),
            "budget-without-timeout-source": (lambda: managed_mixed_root(self, cause="budget"),
                                              "not proven: lifetime_closure_unproven"),
        }
        for label, (arrange, message) in cases.items():
            with self.subTest(label=label):
                acceptance = acceptance_for(arrange(), {"fresh": TASK})
                with self.assertRaisesRegex(AcceptanceFailure, message):
                    lifecycle_impl.record_lifetime_closure(acceptance)
                self.assertIsNone(acceptance.lifetime_closure)

    def test_timeout_source_alone_may_close_by_budget(self):
        acceptance = acceptance_for(managed_mixed_root(self, cause="budget"), {"timeout-source": TASK})
        lifecycle_impl.record_lifetime_closure(acceptance)
        self.assertEqual(acceptance.lifetime_closure["ordinary"]["tasks"][TASK]["cause"], "budget")

    def test_custom_task_or_queue_owned_inspection_is_not_exact_coverage(self):
        custom = InspectionFixture(self)
        custom.ordinary_only_admission("custom")
        with self.assertRaisesRegex(AcceptanceFailure, "lifetime_closure_incomplete"):
            lifecycle_impl.record_lifetime_closure(acceptance_for(custom, {"fresh": TASK}))

        queued = InspectionFixture(self)
        queued.inspection_records()
        ordinary = queued.durable_ordinary_admission(runner_ownership="managed")
        queued.managed_ordinary_lifetime(ordinary)
        with self.assertRaisesRegex(AcceptanceFailure, "inspection lifetime coverage is not exact"):
            lifecycle_impl.record_lifetime_closure(acceptance_for(queued, {"fresh": TASK}))

    def test_known_no_launch_differs_from_an_unknown_attempt(self):
        model_fixture = InspectionFixture(self)
        model_fixture.inspection_records(include_ordinary=False, managed_exit_proof=True)
        ordinary_state = model_fixture.base / "ordinary-state"
        ordinary_state.mkdir()
        empty = ClosureRoot(state=ordinary_state, base=model_fixture.base)
        acceptance = acceptance_for(empty, {}, models_state=model_fixture.state, model_task=TASK)
        lifecycle_impl.record_lifetime_closure(acceptance)
        self.assertEqual(acceptance.lifetime_closure["ordinary"],
                         {"root_id": None, "launch": "none", "tasks": {}, "inspections": {}})
        self.assertEqual(set(acceptance.lifetime_closure["model_discovery"]["inspections"]), {TASK})

        wrong_task = acceptance_for(empty, {}, models_state=model_fixture.state, model_task="c" * 32)
        with self.assertRaisesRegex(AcceptanceFailure, "does not cover its inspection"):
            lifecycle_impl.record_lifetime_closure(wrong_task)

        unknown = acceptance_for(empty, {}, models_state=model_fixture.state, model_task=TASK)
        unknown.ops.dispatch_attempts = {"d" * 32}
        with self.assertRaisesRegex(AcceptanceFailure, "root record"):
            lifecycle_impl.record_lifetime_closure(unknown)

    def test_an_uninspectable_inventory_is_not_a_known_no_launch(self):
        fixture = InspectionFixture(self)
        empty = ClosureRoot(state=fixture.base / "ordinary-state", base=fixture.base)
        empty.state.mkdir()
        for label, target in (("ordinary tasks", empty.state / "tasks"),
                              ("ordinary inspections", empty.state / "inspections"),
                              ("undeclared discovery", fixture.base / "models-state" / "inspections")):
            with self.subTest(inventory=label):
                acceptance = acceptance_for(empty, {})
                acceptance.ops.dispatch_attempts = set()
                denied = PermissionError(13, "injected permission denied")
                with failing_lstat(target, denied), self.assertRaises(PermissionError) as raised:
                    lifecycle_impl.record_lifetime_closure(acceptance)
                self.assertIs(raised.exception, denied)
                self.assertIsNone(acceptance.lifetime_closure)

    def test_model_discovery_root_must_prove_its_own_inspection(self):
        fixture = managed_mixed_root(self)
        models = InspectionFixture(self)
        models.inspection_records(include_ordinary=False, managed_exit_proof=True)
        (models.state / "inspections" / TASK / "lifetime-worker-wait.json").unlink()
        acceptance = acceptance_for(fixture, {"fresh": TASK}, models_state=models.state, model_task=TASK)
        with self.assertRaisesRegex(AcceptanceFailure, "inspection lifetime worker Wait"):
            lifecycle_impl.record_lifetime_closure(acceptance)
        shutil.rmtree(models.state / "inspections")
        with self.assertRaisesRegex(AcceptanceFailure, "lifetime_closure_incomplete"):
            lifecycle_impl.record_lifetime_closure(acceptance)
        undeclared = acceptance_for(fixture, {"fresh": TASK})
        (undeclared.models_state / "inspections").mkdir(parents=True)
        with self.assertRaisesRegex(AcceptanceFailure, "without a discovery result"):
            lifecycle_impl.record_lifetime_closure(undeclared)

    def test_internal_custom_fixture_records_no_proof(self):
        acceptance = acceptance_for(InspectionFixture(self), {"fresh": TASK}, managed=False)
        lifecycle_impl.record_lifetime_closure(acceptance)
        self.assertIsNone(acceptance.lifetime_closure)
        self.assertEqual(lifecycle_impl.lifecycle_receipt(acceptance),
                         {"natural_daemon_shutdown": False, "lifetime_closure": None})


class ShutdownOrderingTests(unittest.TestCase):
    def shutdown_owner(self, fixture: InspectionFixture) -> tuple[SimpleNamespace, list[str]]:
        acceptance = acceptance_for(fixture, {"fresh": TASK})
        calls: list[str] = []
        acceptance.daemon = Mock()
        acceptance.pueue, acceptance.config = Path("/pueue"), Path("/pueue.yml")
        acceptance.direct = lambda name, *_args, **_kwargs: calls.append(name)
        acceptance.terminal_private_queue = lambda _status: True
        return acceptance, calls

    def test_closure_is_proven_after_terminal_queue_and_before_daemon_shutdown(self):
        acceptance, calls = self.shutdown_owner(managed_mixed_root(self))
        with patch.object(lifecycle_impl, "status_jobs", return_value={}):
            lifecycle_impl.shutdown(acceptance)
        self.assertEqual(calls, ["final-status", "private-shutdown"])
        self.assertTrue(acceptance.closed)
        self.assertEqual(lifecycle_impl.lifecycle_receipt(acceptance),
                         {"natural_daemon_shutdown": True,
                          "lifetime_closure": acceptance.lifetime_closure})

    def test_unproven_closure_keeps_the_daemon_and_resources(self):
        acceptance, calls = self.shutdown_owner(managed_mixed_root(self, include_wait=False))
        with patch.object(lifecycle_impl, "status_jobs", return_value={}), \
                self.assertRaisesRegex(AcceptanceFailure, "Wait is missing"):
            lifecycle_impl.shutdown(acceptance)
        self.assertEqual(calls, ["final-status"])
        acceptance.daemon.wait.assert_not_called()
        self.assertFalse(acceptance.closed)
        self.assertTrue((acceptance.state / "tasks" / TASK).is_dir())


if __name__ == "__main__":
    unittest.main()
