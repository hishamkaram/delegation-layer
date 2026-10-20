"""A resolved PASS requires the exact bound managed lifetime closure proof."""

from copy import deepcopy
import json
from pathlib import Path
import tempfile
import unittest

from scripts import native_acceptance_evidence as evidence

from scripts.acceptance_tests.native.support import lifetime_closure_fact, lifetime_closure_source
from scripts.delegate_acceptance.native_evidence import contract
from scripts.delegate_acceptance.native_evidence.lifetime_closure import lifetime_closure_error
# Import the module, not the TestCase, so its tests are discovered only once.
from scripts import test_native_acceptance_evidence as evidence_tests


FRESH, RESUME, MODEL = "1" * 32, "2" * 32, "3" * 32
TASKS = {"fresh": FRESH, "resume": RESUME}


def source(tasks=None, **changes):
    tasks = dict(TASKS if tasks is None else tasks)
    return {"tasks": tasks, "natural_daemon_shutdown": True,
            "lifetime_closure": lifetime_closure_source(tasks), **changes}


class LifetimeClosureRuleTests(unittest.TestCase):
    def test_complete_and_timeout_budget_closures_pass(self):
        self.assertIsNone(lifetime_closure_error(lifetime_closure_source(TASKS), task_ids=TASKS.values()))
        timeout = {"timeout_source": FRESH, "timeout_resume": RESUME}
        self.assertIsNone(lifetime_closure_error(
            lifetime_closure_source(timeout), task_ids=timeout.values(), budget_task_ids=[FRESH]))
        self.assertEqual(lifetime_closure_error(
            lifetime_closure_source(timeout), task_ids=timeout.values()), "lifetime_closure_unproven")

    def test_missing_partial_empty_or_unproven_facts_never_pass(self):
        def mutate(change):
            proof = lifetime_closure_source(TASKS)
            change(proof)
            return lifetime_closure_error(proof, task_ids=TASKS.values())

        def ordinary(proof):
            return proof["ordinary"]

        cases = {
            "partial": (lambda proof: ordinary(proof)["tasks"].pop(RESUME), "lifetime_closure_incomplete"),
            "empty": (lambda proof: ordinary(proof).update(tasks={}), "lifetime_closure_incomplete"),
            "extra": (lambda proof: ordinary(proof)["tasks"].update({MODEL: lifetime_closure_fact()}),
                      "lifetime_closure_incomplete"),
            "missing-wait": (lambda proof: ordinary(proof)["tasks"][FRESH].update(wait_result=None),
                             "lifetime_closure_unproven"),
            "recovered": (lambda proof: ordinary(proof)["tasks"][FRESH].update(recovered=True),
                          "lifetime_closure_unproven"),
            "wait-error": (lambda proof: ordinary(proof)["tasks"][FRESH].update(wait_result="wait-error"),
                           "lifetime_closure_unproven"),
            "scope": (lambda proof: ordinary(proof)["tasks"][FRESH].update(scope_empty=False),
                      "lifetime_closure_unproven"),
            "no-launch-count": (lambda proof: ordinary(proof)["tasks"][FRESH].update(launch_count=0),
                                "lifetime_closure_unproven"),
            "previous-boot": (lambda proof: ordinary(proof)["tasks"][FRESH].update(cause="previous-boot"),
                              "lifetime_closure_unproven"),
            "digest": (lambda proof: ordinary(proof)["tasks"][FRESH].update(wait_sha256=None),
                       "lifetime_closure_invalid"),
            "raw-field": (lambda proof: ordinary(proof)["tasks"][FRESH].update(pid=7),
                          "lifetime_closure_invalid"),
            "no-launch-claim": (lambda proof: proof.update(ordinary={
                "root_id": None, "launch": "none", "tasks": {}, "inspections": {}}),
                "lifetime_closure_incomplete"),
            "kind": (lambda proof: proof.update(kind="daemon-shutdown"), "lifetime_closure_invalid"),
        }
        self.assertEqual(lifetime_closure_error(None, task_ids=TASKS.values()), "lifetime_closure_missing")
        for label, (change, expected) in cases.items():
            with self.subTest(label=label):
                self.assertEqual(mutate(change), expected)

    def test_model_discovery_root_owns_exactly_its_own_inspection(self):
        proof = lifetime_closure_source({}, model_discovery=MODEL)
        proof["ordinary"] = {"root_id": None, "launch": "none", "tasks": {}, "inspections": {}}
        self.assertIsNone(lifetime_closure_error(proof, task_ids=(), model_discovery_required=True))
        self.assertEqual(lifetime_closure_error(proof, task_ids=(), model_discovery_required=False),
                         "lifetime_closure_incomplete")
        empty = deepcopy(proof)
        empty["model_discovery"]["inspections"] = {}
        self.assertEqual(lifetime_closure_error(empty, task_ids=()), "lifetime_closure_incomplete")
        crossed = deepcopy(proof)
        crossed["model_discovery"]["tasks"] = {FRESH: lifetime_closure_fact()}
        self.assertEqual(lifetime_closure_error(crossed, task_ids=()), "lifetime_closure_incomplete")
        missing = lifetime_closure_source(TASKS)
        self.assertEqual(lifetime_closure_error(missing, task_ids=TASKS.values(),
                                                model_discovery_required=True),
                         "lifetime_closure_incomplete")


class SourceLifecycleTests(unittest.TestCase):
    def summary(self, value, cleanup_ok=True):
        return contract._lifecycle_summary(value, {"status": "verified"}, cleanup_ok, None)

    def test_complete_source_binds_the_private_closure(self):
        value = source()
        lifecycle = self.summary(value)
        self.assertEqual((lifecycle["state"], lifecycle["closure"]), ("complete", value["lifetime_closure"]))
        self.assertEqual(self.summary(value, cleanup_ok=False)["state"], "unresolved")

    def test_daemon_only_or_partial_source_is_unresolved(self):
        partial = source()
        partial["lifetime_closure"]["ordinary"]["tasks"].pop(RESUME)
        discovery = source(model_discovery={"status": "available"})
        for label, value in (("daemon-only", {k: v for k, v in source().items() if k != "lifetime_closure"}),
                             ("partial", partial), ("discovery-missing", discovery),
                             ("no-tasks", source(tasks={}))):
            with self.subTest(label=label):
                lifecycle = self.summary(value)
                self.assertEqual((lifecycle["state"], lifecycle["closure"]), ("unresolved", None))


class CompactIdentityTests(unittest.TestCase):
    def case(self, status="PASS"):
        definition = contract.EXPECTED_CASES[0]
        case = {"provider": definition.provider, "permission": definition.permission,
                "scenario": definition.scenario, "status": status,
                "evidence_status": "RESOLVED" if status == "PASS" else "UNRESOLVED"}
        lifecycle = {"state": "complete", "closure": lifetime_closure_source(TASKS)}
        proof = {
            "state": "complete", "source_receipt_sha256": "a" * 64, "final_status": status,
            "gate": "acceptance-native", "provider": definition.provider,
            "permission": definition.permission, "mode": definition.permission,
            "scenario": definition.scenario, "requested_options_state": "recorded",
            "requested_model": None, "requested_effort": None, "task_ids": sorted(TASKS.values()),
            "task_session_bindings": [{"alias": alias, "task_id": task, "session_id": "s"}
                                      for alias, task in sorted(TASKS.items())],
            "diagnostic_task_ids": [], "session_ids": {}, "binary_identities": {},
            "lifecycle_proof": lifecycle,
        }
        return case, {"source_proof": proof}

    def test_resolved_pass_requires_exact_bound_closure(self):
        case, compact = self.case()
        self.assertIs(contract._validate_case_proof_identity(case, compact), compact["source_proof"])
        for label, change, expected in (
                ("missing", lambda proof: proof["lifecycle_proof"].pop("closure"),
                 "aggregate_case_lifetime_closure_missing"),
                ("partial", lambda proof: proof["lifecycle_proof"]["closure"]["ordinary"]["tasks"].pop(RESUME),
                 "aggregate_case_lifetime_closure_incomplete"),
                ("rebound", lambda proof: proof["task_session_bindings"][0].update(task_id="5" * 32),
                 "aggregate_case_lifetime_closure_incomplete")):
            with self.subTest(label=label):
                case, compact = self.case()
                change(compact["source_proof"])
                with self.assertRaisesRegex(contract.EvidenceError, expected):
                    contract._validate_case_proof_identity(case, compact)

    def test_failure_cleanup_is_not_held_to_the_positive_closure_rule(self):
        case, compact = self.case(status="FAIL")
        compact["source_proof"]["lifecycle_proof"] = {"state": "unresolved", "closure": None}
        self.assertIs(contract._validate_case_proof_identity(case, compact), compact["source_proof"])


class ClosureAggregateTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-closure-aggregate-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.fixture = object.__new__(evidence_tests.NativeAcceptanceEvidenceTests)
        self.fixture.root = self.root

    def test_pass_requires_the_bound_lifetime_closure_before_raw_cleanup(self):
        def without_closure(value):
            value.pop("lifetime_closure")

        def partial(value):
            value["lifetime_closure"]["ordinary"]["tasks"].pop(value["tasks"]["resume"])

        def missing_wait(value):
            value["lifetime_closure"]["ordinary"]["tasks"][value["tasks"]["fresh"]]["wait_result"] = None

        def foreign_budget(value):
            value["lifetime_closure"]["ordinary"]["tasks"][value["tasks"]["fresh"]]["cause"] = "budget"

        def empty(value):
            value["lifetime_closure"]["ordinary"]["tasks"] = {}

        for label, change in (("daemon-only", without_closure), ("partial", partial),
                              ("missing-wait", missing_wait), ("foreign-budget", foreign_budget),
                              ("empty", empty)):
            with self.subTest(label=label):
                root = self.root / f"native-aggregate-closure-{label}"
                matrix = self.fixture.make_matrix(root, status="PASS")
                case_directory = root / evidence.EXPECTED_CASES[0].case_output
                source = case_directory / "success.json"
                value = json.loads(source.read_text(encoding="utf-8"))
                change(value)
                source.write_text(json.dumps(value), encoding="utf-8")
                aggregate, code = evidence.build_aggregate(
                    root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
                    tag="v1.2.3", source_revision="a" * 40,
                )
                self.assertEqual(code, 1)
                self.assertEqual(aggregate["cases"][0]["evidence_status"], "UNRESOLVED")
                compact = json.loads(Path(aggregate["cases"][0]["receipt_path"]).read_text(encoding="utf-8"))
                lifecycle = compact["source_proof"]["lifecycle_proof"]
                self.assertEqual((lifecycle["state"], lifecycle["closure"]), ("unresolved", None))
                self.assertTrue(case_directory.is_dir())

    def test_pass_retains_the_compact_closure_and_accepts_the_timeout_source_budget(self):
        root = self.root / "native-aggregate-closure-pass"
        value, code = self.fixture.build(root=root, status="PASS")
        self.assertEqual(code, 0)
        for case in value["cases"]:
            compact = json.loads(Path(case["receipt_path"]).read_text(encoding="utf-8"))
            proof = compact["source_proof"]
            closure = proof["lifecycle_proof"]["closure"]
            self.assertEqual(set(closure["ordinary"]["tasks"]), set(proof["task_ids"]))
            if case["scenario"] == "timeout-resume":
                aliases = {binding["alias"]: binding["task_id"] for binding in proof["task_session_bindings"]}
                self.assertEqual(closure["ordinary"]["tasks"][aliases["timeout_source"]]["cause"], "budget")


if __name__ == "__main__":
    unittest.main()
