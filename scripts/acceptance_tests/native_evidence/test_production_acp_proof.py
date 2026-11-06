"""Production OpenCode ACP facts must survive into the canonical release proof."""

from copy import deepcopy
import json
from pathlib import Path
import tempfile
import unittest

from scripts import native_acceptance_evidence as evidence
from scripts.acceptance_tests.native.support import production_acp_source
from scripts.delegate_acceptance.native_evidence.contract import (
    EvidenceError,
    _validate_case_binaries,
    _validate_case_completion,
    _validate_case_proof_identity,
    _validate_case_sessions,
)
from scripts.delegate_acceptance.native_evidence.production_acp import (
    ACP_PREDICATE_VERSION,
    ACP_PROFILE_REVISION,
    compact_production_acp,
    production_acp_proof_error,
)
# Import the module, not the TestCase, so its tests are discovered only once.
from scripts import test_native_acceptance_evidence as evidence_tests


HISTORICAL_CASE_IDS = (
    "case-01-antigravity-print-workspace-write-write",
    "case-02-claude-print-workspace-write-write",
    "case-03-ccr-print-workspace-write-write",
    "case-04-codex-exec-workspace-write-write",
    "case-05-pi-json-workspace-write-write",
    "case-06-opencode-run-workspace-write-write",
    "case-07-codex-exec-workspace-write-timeout-resume",
    "case-08-antigravity-print-read-only-read-only",
    "case-09-claude-print-read-only-read-only",
    "case-10-ccr-print-read-only-read-only",
    "case-11-codex-exec-read-only-read-only",
    "case-12-pi-json-read-only-read-only",
    "case-13-opencode-run-read-only-read-only",
    "case-14-codex-review-read-only-read-only",
    "case-15-codex-exec-read-only-non-git",
    "case-16-codex-exec-workspace-write-non-git",
)
FRESH, RESUME = "1" * 32, "2" * 32
SOURCE, SUCCESSOR = "3" * 32, "4" * 32


def object_field(value: dict[str, object], key: str) -> dict[str, object]:
    field = value[key]
    assert isinstance(field, dict)
    return field


def source_turn(value: dict[str, object], alias: str) -> dict[str, object]:
    return object_field(object_field(value, "tasks"), alias)


def historical(marker: dict[str, object], *aliases: str, envelope: bool = True) -> dict[str, object]:
    """Return a copy whose envelope and the named turns carry the retired predicate."""
    value = deepcopy(marker)
    if envelope:
        value["predicate_version"] = "acp-v1"
    for alias in aliases:
        source_turn(value, alias)["predicate_version"] = "acp-v1"
    return value


def bindings(tasks: dict[str, str]) -> list[dict[str, str]]:
    return [{"alias": alias, "task_id": task_id, "session_id": "session-1"}
            for alias, task_id in sorted(tasks.items())]


class ProductionAcpProofTests(unittest.TestCase):
    def compact(self, marker, tasks, aliases):
        return compact_production_acp({"production_acp": marker}, bindings(tasks), aliases)

    def test_matrix_appends_only_the_opencode_timeout_row(self):
        cases = evidence.EXPECTED_CASES
        self.assertEqual(len(cases), 17)
        self.assertEqual(tuple(case.case_id for case in cases[:16]), HISTORICAL_CASE_IDS)
        self.assertEqual(
            (cases[16].case_id, cases[16].provider, cases[16].permission, cases[16].scenario),
            ("case-17-opencode-run-workspace-write-timeout-resume", "opencode:run",
             "workspace-write", "timeout-resume"))
        self.assertEqual(
            [(case.provider, case.permission) for case in cases if case.scenario == "timeout-resume"],
            [("codex:exec", "workspace-write"), ("opencode:run", "workspace-write")])

    def test_release_pins_are_the_current_predicate_and_unchanged_profile(self):
        self.assertEqual((ACP_PROFILE_REVISION, ACP_PREDICATE_VERSION),
                         ("opencode-acp-v1", "acp-v2"))

    def test_complete_marker_binds_every_turn_to_its_sealed_task(self):
        for tasks, aliases in (({"fresh": FRESH, "resume": RESUME}, ("fresh", "resume")),
                               ({"timeout_source": SOURCE, "timeout_resume": SUCCESSOR},
                                ("timeout_resume", "timeout_source"))):
            with self.subTest(aliases=aliases):
                marker = production_acp_source(tasks, {alias: "session-1" for alias in tasks})
                proof = self.compact(marker, tasks, aliases)
                self.assertEqual(proof["state"], "complete")
                self.assertEqual(proof["profile_revision"], "opencode-acp-v1")
                self.assertEqual(proof["predicate_version"], "acp-v2")
                self.assertEqual({turn["alias"]: turn["publication"] for turn in proof["tasks"]},
                                 {alias: "unpublished" if alias == "timeout_source" else "committed"
                                  for alias in aliases})
                self.assertEqual([turn["alias"] for turn in proof["tasks"]], sorted(aliases))
                self.assertEqual(proof, self.compact(deepcopy(marker), tasks, aliases))
                self.assertNotIn("session_id", json.dumps(proof["tasks"]))
                case = {"provider": "opencode:run", "status": "PASS"}
                self.assertIsNone(production_acp_proof_error(
                    case["provider"], {"production_acp": proof,
                                       "task_session_bindings": bindings(tasks)},
                    resolved_pass=True, expected_aliases=aliases))

    def test_historical_or_tampered_markers_never_become_production_acp(self):
        tasks = {"fresh": FRESH, "resume": RESUME}
        aliases = ("fresh", "resume")
        self.assertEqual(compact_production_acp({}, bindings(tasks), aliases), {"state": "missing"})
        tampers = {
            "legacy run revision": ("fresh", "profile_revision", "opencode-native-v2"),
            "wrong predicate": ("resume", "predicate_version", "native-v1"),
            "interactive": ("fresh", "noninteractive", False),
            "interaction journal": ("resume", "interaction_journal", "present"),
            "wrong session": ("resume", "session_id", "session-2"),
            "wrong task": ("resume", "task_id", "5" * 32),
            "wrong predecessor": ("resume", "predecessor_task_id", "5" * 32),
            "fresh predecessor": ("fresh", "predecessor_task_id", RESUME),
            "missing seal": ("fresh", "seal_sha256", None),
            "unpublished commit": ("resume", "publication", "unpublished"),
            "predicate drift": ("resume", "predicate_sha256", "6" * 64),
            "malformed digest": ("fresh", "meta_sha256", "not-a-digest"),
        }
        for label, (alias, key, value) in tampers.items():
            with self.subTest(label=label):
                marker = production_acp_source(tasks, {alias: "session-1" for alias in tasks})
                source_turn(marker, alias)[key] = value
                self.assertEqual(self.compact(marker, tasks, aliases), {"state": "invalid"})
        for label, mutate in {
                "extra field": lambda marker: source_turn(marker, "fresh").update(stdout="raw"),
                "missing turn": lambda marker: marker["tasks"].pop("resume"),
                "wrong kind": lambda marker: marker.update(kind="opencode-acp-probe"),
                "string schema": lambda marker: marker.update(schema_version="1"),
        }.items():
            with self.subTest(label=label):
                marker = production_acp_source(tasks, {alias: "session-1" for alias in tasks})
                mutate(marker)
                self.assertEqual(self.compact(marker, tasks, aliases), {"state": "invalid"})

    def test_retired_predicate_is_invalid_for_every_turn_and_envelope_mix(self):
        for tasks, aliases in (({"fresh": FRESH, "resume": RESUME}, ("fresh", "resume")),
                               ({"timeout_source": SOURCE, "timeout_resume": SUCCESSOR},
                                ("timeout_resume", "timeout_source"))):
            marker = production_acp_source(tasks, {alias: "session-1" for alias in tasks})
            self.assertEqual(self.compact(marker, tasks, aliases)["state"], "complete")
            mixes = {"all retired": historical(marker, *aliases),
                     "retired envelope only": historical(marker),
                     **{f"only {alias} retired": historical(marker, alias, envelope=False)
                        for alias in aliases},
                     **{f"retired envelope, {alias} current": {
                         **historical(marker, *(other for other in aliases if other != alias))}
                        for alias in aliases}}
            for label, value in mixes.items():
                with self.subTest(aliases=aliases, mix=label):
                    self.assertEqual(self.compact(value, tasks, aliases), {"state": "invalid"})

    def test_container_predicate_digest_is_invalid_not_a_crash(self):
        tasks = {"fresh": FRESH, "resume": RESUME}
        aliases = ("fresh", "resume")
        control = production_acp_source(tasks, {alias: "session-1" for alias in tasks})
        self.assertEqual(self.compact(control, tasks, aliases)["state"], "complete")
        for value in ([], {}, ["7" * 64], {"sha256": "7" * 64}):
            for alias in aliases:
                with self.subTest(value=value, alias=alias):
                    marker = deepcopy(control)
                    source_turn(marker, alias)["predicate_sha256"] = value
                    self.assertEqual(self.compact(marker, tasks, aliases), {"state": "invalid"})

    def test_non_string_compact_state_is_invalid_not_a_crash(self):
        tasks = {"fresh": FRESH, "resume": RESUME}
        aliases = ("fresh", "resume")
        complete = self.compact(
            production_acp_source(tasks, {alias: "session-1" for alias in tasks}), tasks, aliases)
        self.assertIsNone(production_acp_proof_error(
            "opencode:run", {"production_acp": complete, "task_session_bindings": bindings(tasks)},
            resolved_pass=True, expected_aliases=aliases))
        for state in ([], {}, ["complete"], {"complete": True}, 1, None):
            for resolved_pass in (True, False):
                with self.subTest(state=state, resolved_pass=resolved_pass):
                    proof = {"production_acp": {"state": state},
                             "task_session_bindings": bindings(tasks)}
                    self.assertEqual(production_acp_proof_error(
                        "opencode:run", proof, resolved_pass=resolved_pass,
                        expected_aliases=aliases), "aggregate_case_production_acp_invalid")

    def test_timeout_source_must_remain_unpublished(self):
        tasks = {"timeout_source": SOURCE, "timeout_resume": SUCCESSOR}
        aliases = ("timeout_resume", "timeout_source")
        for key, value in (("publication", "committed"), ("outcome_sha256", "c" * 64)):
            with self.subTest(key=key):
                marker = production_acp_source(tasks, {alias: "session-1" for alias in tasks})
                source_turn(marker, "timeout_source")[key] = value
                self.assertEqual(self.compact(marker, tasks, aliases), {"state": "invalid"})

    def test_case_validator_requires_complete_bound_opencode_proof(self):
        tasks = {"fresh": FRESH, "resume": RESUME}
        aliases = ("fresh", "resume")
        marker = production_acp_source(tasks, {alias: "session-1" for alias in tasks})
        complete = self.compact(marker, tasks, aliases)
        proof = {"production_acp": complete, "task_session_bindings": bindings(tasks)}

        def error(provider, value, resolved_pass=True):
            return production_acp_proof_error(
                provider, value, resolved_pass=resolved_pass, expected_aliases=aliases)

        self.assertEqual(error("opencode:run", {"task_session_bindings": bindings(tasks)}),
                         "aggregate_case_production_acp_invalid")
        self.assertEqual(error("opencode:run", {**proof, "production_acp": {"state": "missing"}}),
                         "aggregate_case_production_acp_incomplete")
        self.assertIsNone(error("opencode:run", {"task_session_bindings": []}, resolved_pass=False))
        self.assertIsNone(error("opencode:run", {**proof, "production_acp": {"state": "invalid"}},
                                resolved_pass=False))
        self.assertEqual(error("codex:exec", proof), "aggregate_case_production_acp_unexpected")
        self.assertIsNone(error("codex:exec", {"task_session_bindings": bindings(tasks)}))
        rebound = {**proof, "task_session_bindings": bindings({"fresh": FRESH, "resume": "5" * 32})}
        self.assertEqual(error("opencode:run", rebound), "aggregate_case_production_acp_unbound")
        relabeled = deepcopy(proof)
        object_field(relabeled, "production_acp")["profile_revision"] = "opencode-native-v2"
        self.assertEqual(error("opencode:run", relabeled), "aggregate_case_production_acp_invalid")
        # Every other fact is valid and bound; only the retired predicate is wrong.
        retired = deepcopy(proof)
        object_field(retired, "production_acp")["predicate_version"] = "acp-v1"
        self.assertEqual(error("opencode:run", retired), "aggregate_case_production_acp_invalid")
        self.assertEqual(error("opencode:run", proof), None)


class ProductionAcpAggregateTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="native-production-acp-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve(strict=True)
        self.fixture = object.__new__(evidence_tests.NativeAcceptanceEvidenceTests)

    def build(self, name, mutate=None):
        root = self.root / name
        matrix = self.fixture.make_matrix(root, status="PASS")
        if mutate is not None:
            source = root / "cases" / "case-06-opencode-run-workspace-write-write" / "success.json"
            value = json.loads(source.read_text(encoding="utf-8"))
            mutate(value)
            source.write_text(json.dumps(value), encoding="utf-8")
        aggregate, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40, require_pass=True)
        return root, aggregate, code

    @staticmethod
    def compact_case(aggregate, case_id):
        case = next(case for case in aggregate["cases"] if case["case_id"] == case_id)
        return case, json.loads(Path(case["receipt_path"]).read_text(encoding="utf-8"))

    def test_every_opencode_pass_carries_complete_production_acp_proof(self):
        root, aggregate, code = self.build("complete")
        self.assertEqual((code, aggregate["status"]), (0, "PASS"))
        evidence.validate_aggregate_receipt(root / evidence.AGGREGATE_RECEIPT_NAME)
        for definition in evidence.EXPECTED_CASES:
            case, compact = self.compact_case(aggregate, definition.case_id)
            proof = compact["source_proof"]
            if definition.provider != "opencode:run":
                self.assertNotIn("production_acp", proof)
                continue
            with self.subTest(case=definition.case_id):
                self.assertEqual(proof["production_acp"]["state"], "complete")
                self.assertEqual({turn["task_id"] for turn in proof["production_acp"]["tasks"]},
                                 set(proof["task_ids"]))
                stripped = deepcopy(compact)
                stripped["source_proof"].pop("production_acp")
                with self.assertRaisesRegex(EvidenceError, "aggregate_case_production_acp_invalid"):
                    _validate_case_proof_identity(case, stripped)

    def test_container_acp_values_resolve_to_explicit_aggregate_failures(self):
        for value in ([], {}):
            with self.subTest(value=value):
                _root, aggregate, code = self.build(
                    f"container-{type(value).__name__}",
                    lambda source, value=value: source["production_acp"]["tasks"]["fresh"].update(
                        predicate_sha256=value))
                self.assertEqual((code, aggregate["status"]), (1, "FAIL"))
                case, compact = self.compact_case(
                    aggregate, "case-06-opencode-run-workspace-write-write")
                self.assertEqual(case["evidence_status"], "UNRESOLVED")
                self.assertEqual(compact["evidence_error"], "source_production_acp_invalid")
        _root, aggregate, code = self.build("container-state")
        self.assertEqual(code, 0)
        case, compact = self.compact_case(aggregate, "case-06-opencode-run-workspace-write-write")
        for state in ([], {}):
            with self.subTest(state=state):
                tampered = deepcopy(compact)
                tampered["source_proof"]["production_acp"]["state"] = state
                with self.assertRaisesRegex(EvidenceError, "aggregate_case_production_acp_invalid"):
                    _validate_case_proof_identity(case, tampered)

    def test_retired_predicate_source_cannot_claim_pass(self):
        def retire(value, *aliases, envelope=True):
            value["production_acp"] = historical(value["production_acp"], *aliases, envelope=envelope)

        for name, mutate in (
                ("all retired", lambda value: retire(value, "fresh", "resume")),
                ("retired envelope only", lambda value: retire(value)),
                ("one retired task", lambda value: retire(value, "resume", envelope=False))):
            with self.subTest(source=name):
                _root, aggregate, code = self.build(name.replace(" ", "-"), mutate)
                self.assertEqual((code, aggregate["status"]), (1, "FAIL"))
                case, compact = self.compact_case(
                    aggregate, "case-06-opencode-run-workspace-write-write")
                self.assertEqual(case["evidence_status"], "UNRESOLVED")
                self.assertEqual(compact["evidence_error"], "source_production_acp_invalid")
                self.assertEqual(compact["source_proof"]["state"], "unresolved")
                self.assertEqual(compact["source_proof"]["production_acp"], {"state": "invalid"})

    def test_retired_predicate_failed_source_stays_unresolved_with_evidence_retained(self):
        root = self.root / "retired-failure"
        matrix = self.fixture.make_matrix(root, status="PASS", failed_index=5)
        case_directory = root / evidence.EXPECTED_CASES[5].case_output
        aliases = ("fresh", "resume")
        tasks = {alias: task_id for alias, task_id in zip(aliases, (FRESH, RESUME))}
        sessions = {alias: "session-1" for alias in aliases}
        failure = case_directory / "failure.json"
        value = json.loads(failure.read_text(encoding="utf-8"))
        value.update(tasks=tasks, provider_sessions=sessions, production_acp=historical(
            production_acp_source(tasks, sessions), *aliases))
        failure.write_text(json.dumps(value), encoding="utf-8")
        source_bytes = failure.read_bytes()
        raw = case_directory / "raw-provider-output"
        raw_bytes = raw.read_bytes()

        aggregate, code = evidence.build_aggregate(
            root, matrix, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag="v1.2.3", source_revision="a" * 40, require_pass=True)
        self.assertEqual((code, aggregate["status"], aggregate["unresolved_case_count"]),
                         (1, "FAIL", 1))
        self.assertEqual((failure.read_bytes(), raw.read_bytes()), (source_bytes, raw_bytes))
        case, compact = self.compact_case(aggregate, evidence.EXPECTED_CASES[5].case_id)
        proof = compact["source_proof"]
        self.assertEqual((case["evidence_status"], proof["state"], proof["final_status"]),
                         ("UNRESOLVED", "unresolved", "FAIL"))
        self.assertEqual(proof["source_receipt_sha256"], evidence.sha(source_bytes))
        self.assertEqual(proof["production_acp"], {"state": "invalid"})
        checked = _validate_case_proof_identity(dict(case), compact)
        sessions_proof = _validate_case_sessions(checked)
        binaries = _validate_case_binaries(dict(case), checked)
        _validate_case_completion(dict(case), checked, sessions_proof, binaries)
        evidence.validate_aggregate_receipt(root / evidence.AGGREGATE_RECEIPT_NAME)

    def test_historical_or_tampered_opencode_source_is_explicitly_unresolved(self):
        for name, mutate, reason in (
                ("historical", lambda value: value.pop("production_acp"),
                 "source_production_acp_missing"),
                ("tampered", lambda value: value["production_acp"]["tasks"]["resume"].update(
                    session_id="other-session"), "source_production_acp_invalid")):
            with self.subTest(source=name):
                _root, aggregate, code = self.build(name, mutate)
                self.assertEqual((code, aggregate["status"]), (1, "FAIL"))
                case, compact = self.compact_case(
                    aggregate, "case-06-opencode-run-workspace-write-write")
                self.assertEqual(case["evidence_status"], "UNRESOLVED")
                self.assertEqual(compact["evidence_error"], reason)
                self.assertEqual(compact["source_proof"]["state"], "unresolved")
