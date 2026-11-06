"""Source-proof policy stays independent of filesystem cleanup."""

from copy import deepcopy
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest

from scripts.acceptance_tests.native.support import lifetime_closure_source
from scripts.delegate_acceptance.evidence.files import read_json, write_json
from scripts.delegate_acceptance.native.acceptance_operations import write_binding
from scripts.delegate_acceptance.native.bindings import ProviderRuntimeBindings
from scripts.delegate_acceptance.native_evidence import contract
from scripts.delegate_acceptance.native_evidence import proof as proof_impl
from scripts.delegate_acceptance.native_evidence import validation


class RuntimeReceiptCompatibilityTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-runtime-compatibility-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.row = contract.MatrixRow(contract.EXPECTED_CASES[0], 0, "PASS")
        self.task_id = "a" * 32
        self.tasks = {"fresh": self.task_id, "resume": "b" * 32}
        self.bindings = ProviderRuntimeBindings()
        definition = self.row.definition
        owned = {}
        for name in ("delegate", "runner", "pueue", "pueued"):
            path = self.root / name
            path.write_bytes(name.encode())
            owned[name] = path
        self.owner = SimpleNamespace(
            output=self.root, _provider_bindings=self.bindings, provider=definition.provider,
            mode=definition.permission, scenario=definition.scenario,
            provider_version="admission-v1", native_mode_receipt=lambda: None,
            pueue_version="4.0.4", pueued_version="4.0.4", **owned)

    def produce(self, diagnostics=None):
        meta: dict[str, object] = {"provider_executable": "/admitted/provider", "provider_version": ""}
        for task_id in self.tasks.values():
            started: dict[str, object] = {"provider_executable": "/actual/provider", **(diagnostics or {})}
            identity = self.bindings.record(task_id, meta, Path("/admitted/provider"),
                                             launch_evidence=(started, dict(started)))
            if task_id == self.task_id:
                write_binding(self.owner, task_id, identity)
        return {
            "gate": "acceptance-native", "status": "passed", "provider": self.owner.provider,
            "mode": self.owner.mode, "scenario": self.owner.scenario,
            "requested_model": None, "requested_effort": None,
            "tasks": self.tasks, "provider_sessions": {alias: "session" for alias in self.tasks},
            "natural_daemon_shutdown": True, "lifetime_closure": lifetime_closure_source(self.tasks),
            **self.bindings.receipt(set(self.tasks.values())),
        }

    def source_proof(self, source):
        return proof_impl._source_proof(self.row, source,
            {"status": "verified", "supervisor_shutdown_observed": True}, self.root, True, "c" * 64)

    def validate_binaries(self, binaries):
        return contract._validate_case_binaries(
            {"binary_identities": binaries}, {"binary_identities": binaries})

    def validate_proof(self, proof):
        definition = self.row.definition
        case = {"case_id": definition.case_id, "provider": definition.provider,
                "permission": definition.permission, "scenario": definition.scenario,
                "status": "PASS", "exit_code": 0, "evidence_status": "RESOLVED",
                "binary_identities": proof["binary_identities"]}
        return validation.validate_case_proof(case, {
            **case, "schema_version": 1, "kind": "native-acceptance-case", "source_proof": proof})

    def test_producer_to_private_proof_accepts_absent_diagnostics(self):
        source = self.produce()
        proof = self.source_proof(source)
        self.assertEqual(proof["state"], "complete")
        binaries = proof["binary_identities"]
        self.assertEqual(binaries["provider_binding"], {
            "task_id": self.task_id, "executable": "/actual/provider", "version": ""})
        self.assertNotIn("sha256", binaries)
        self.validate_binaries(binaries)
        self.assertEqual(self.validate_proof(proof), proof)

    def assert_version_map_forms(self, source):
        versions = source["provider_versions"]
        aliases = {alias: versions[task_id] for alias, task_id in self.tasks.items()}
        forms = (versions, aliases, {"fresh": aliases["fresh"]}, {},
                 {"fresh": aliases["fresh"], self.tasks["resume"]: aliases["resume"]},
                 {**versions, **aliases})
        for version_map in forms:
            with self.subTest(versions=version_map):
                changed = deepcopy(source)
                changed["provider_versions"] = version_map
                original = deepcopy(changed)
                proof = self.source_proof(changed)
                self.assertEqual(proof["state"], "complete")
                projected = proof["binary_identities"].get("versions", {})
                self.assertEqual({key: value for key, value in projected.items()
                                  if key.startswith("provider:")},
                                 {f"provider:{key}": value for key, value in version_map.items()})
                self.assertEqual(self.validate_proof(proof), proof)
                self.assertEqual(changed, original)

    def test_alias_and_task_id_version_maps_accept_populated_diagnostics(self):
        self.assert_version_map_forms(self.produce({"provider_version": "runtime-v2"}))

    def test_alias_and_task_id_version_maps_accept_empty_diagnostics(self):
        self.assert_version_map_forms(self.produce())

    def test_invalid_alias_version_diagnostics_refuse_private_proof(self):
        source = self.produce({"provider_version": "runtime-v2"})
        for versions in ({"unknown": "runtime-v2"}, {"fresh": "other-version"},
                         {"fresh": None}, {"fresh": "bad\n"}, {1: "runtime-v2"},
                         {"fresh": "runtime-v2", self.task_id: "other-version"}):
            with self.subTest(versions=versions):
                proof = self.source_proof(dict(source, provider_versions=versions))
                self.assertEqual(proof["state"], "unresolved")
                with self.assertRaises(contract.EvidenceError):
                    self.validate_proof(proof)
        for target in (None, 123, [], "c" * 32, "not-a-task-id"):
            with self.subTest(alias_target=target):
                changed = deepcopy(source)
                changed["provider_versions"] = {"fresh": "runtime-v2"}
                changed["tasks"]["fresh"] = target
                proof = self.source_proof(changed)
                self.assertEqual(proof["state"], "unresolved")
                with self.assertRaises(contract.EvidenceError):
                    self.validate_proof(proof)

    def test_legacy_provider_hash_remains_optional_and_validated(self):
        source = self.produce({"provider_version": "runtime-v2", "provider_sha256": "d" * 64})
        proof = self.source_proof(source)
        self.assertEqual(proof["state"], "complete")
        self.assertNotIn("sha256", proof["binary_identities"])
        self.assertNotIn("provider_sha256", read_json(self.root / "binding.json"))
        self.validate_binaries(proof["binary_identities"])

    def test_missing_compact_diagnostics_and_version_map_are_accepted(self):
        source = self.produce()
        del source["provider_versions"]
        for identity in source["provider_runtime_identities"].values():
            del identity["version"]
            identity.pop("sha256", None)
        binding = read_json(self.root / "binding.json")
        del binding["provider_version"]
        write_json(self.root / "binding.json", binding, replace=True)
        proof = self.source_proof(source)
        self.assertEqual(proof["state"], "complete")
        self.validate_binaries(proof["binary_identities"])

    def test_independently_omitted_diagnostics_do_not_unbind_actual_path(self):
        source = self.produce({"provider_version": "runtime-v2", "provider_sha256": "d" * 64})
        binding = read_json(self.root / "binding.json")
        del binding["provider_version"]
        write_json(self.root / "binding.json", binding, replace=True)
        proof = self.source_proof(source)
        self.assertEqual(proof["state"], "complete")
        self.validate_proof(proof)
        # Private historical projections may omit either optional diagnostic independently.
        binaries = proof["binary_identities"]
        del binaries["provider_runtime_identities"][self.task_id]["version"]
        binaries["provider_runtime_identities"][self.task_id].pop("sha256", None)
        self.validate_binaries(binaries)

    def test_present_invalid_diagnostics_and_paths_refuse_proof(self):
        source = self.produce()
        for field, value in (("version", None), ("version", " \t"), ("version", "bad\n"),
                             ("sha256", None), ("sha256", "bad"),
                             ("executable", "/actual/../provider"), ("executable", "relative")):
            with self.subTest(field=field, value=value):
                changed = deepcopy(source)
                changed["provider_runtime_identities"][self.task_id][field] = value
                self.assertEqual(self.source_proof(changed)["state"], "unresolved")

    def test_owned_selection_and_actual_task_coverage_remain_strict(self):
        source = self.produce()
        binding = read_json(self.root / "binding.json")
        for field in ("delegate", "delegate-run", "pueue", "pueued"):
            for value in (None, "bad"):
                with self.subTest(field=field, value=value):
                    write_json(self.root / "binding.json", dict(binding, installed_executables={**binding["installed_executables"], field: value}), replace=True)
                    self.assertEqual(self.source_proof(source)["state"], "unresolved")
        write_json(self.root / "binding.json", binding, replace=True)
        changed = deepcopy(source)
        del changed["provider_runtime_identities"][self.tasks["resume"]]
        self.assertEqual(self.source_proof(changed)["state"], "unresolved")

    def test_binding_and_version_map_malformed_diagnostics_refuse_proof(self):
        source = self.produce()
        binding = read_json(self.root / "binding.json")
        for field, value in (("provider_version", None), ("provider_version", "bad\n"),
                             ("provider_executable", None), ("provider_executable", "relative")):
            with self.subTest(field=field, value=value):
                write_json(self.root / "binding.json", dict(binding, installed_executables={**binding["installed_executables"], field: value}), replace=True)
                self.assertEqual(self.source_proof(source)["state"], "unresolved")
        write_json(self.root / "binding.json", binding, replace=True)
        for value in (None, {self.task_id: None}, {self.task_id: "bad\n"}, {"c" * 32: ""}):
            with self.subTest(versions=value):
                self.assertEqual(self.source_proof(dict(source, provider_versions=value))[
                    "state"], "unresolved")

    def test_session_root_lifetime_and_closure_tampering_remain_refused(self):
        proof = self.source_proof(self.produce())
        self.validate_proof(proof)
        changed = deepcopy(proof)
        changed["session_ids"]["by_alias"]["resume"] = "other-session"
        with self.assertRaises(contract.EvidenceError):
            self.validate_proof(changed)
        for field, value in (("root_id", "wrong-root"), ("tasks", {})):
            with self.subTest(field=field):
                changed = deepcopy(proof)
                changed["lifecycle_proof"]["closure"]["ordinary"][field] = value
                with self.assertRaises(contract.EvidenceError):
                    self.validate_proof(changed)
        changed = deepcopy(proof)
        changed["lifecycle_proof"]["closure"] = None
        with self.assertRaises(contract.EvidenceError):
            self.validate_proof(changed)

    def test_private_validator_rejects_forged_complete_proof(self):
        source = self.produce()
        binaries = self.source_proof(source)["binary_identities"]
        self.validate_binaries(binaries)
        for field, value in (("version", None), ("sha256", "bad"),
                             ("executable", "/actual/../provider")):
            with self.subTest(field=field):
                changed = deepcopy(binaries)
                changed["provider_runtime_identities"][self.task_id][field] = value
                with self.assertRaises(contract.EvidenceError):
                    self.validate_binaries(changed)
        for field in ("delegate", "delegate-run", "pueue", "pueued"):
            with self.subTest(field=field):
                changed = deepcopy(binaries)
                del changed["paths"][field]
                with self.assertRaisesRegex(contract.EvidenceError, "binary_selection_invalid"):
                    self.validate_binaries(changed)
        for field, value in (("version", None), ("sha256", None), ("sha256", "bad"),
                             ("executable", "relative")):
            with self.subTest(binding_field=field, value=value):
                changed = deepcopy(binaries)
                changed["provider_binding"][field] = value
                with self.assertRaisesRegex(contract.EvidenceError, "provider_binding_invalid"):
                    self.validate_binaries(changed)
        changed = deepcopy(binaries)
        del changed["provider_runtime_identities"][self.tasks["resume"]]
        with self.assertRaisesRegex(contract.EvidenceError, "provider_runtime_coverage_invalid"):
            self.validate_binaries(changed)
        changed = deepcopy(binaries)
        changed["provider_binding"]["executable"] = "/other/provider"
        with self.assertRaisesRegex(contract.EvidenceError, "provider_runtime_coverage_invalid"):
            self.validate_binaries(changed)


class SourceContractTests(unittest.TestCase):
    def setUp(self):
        self.row = contract.MatrixRow(contract.EXPECTED_CASES[0], 0, "PASS")
        definition = self.row.definition
        self.proof = {
            "gate": "acceptance-native", "provider": definition.provider,
            "permission": definition.permission, "mode": definition.permission,
            "scenario": definition.scenario, "requested_options_state": "recorded",
            "state": "complete",
        }

    def test_complete_source_proof_has_no_error(self):
        self.assertIsNone(contract._source_proof_error(self.row, {}, self.proof, None))

    def test_invalid_source_proof_is_rejected_with_the_original_error(self):
        mutations = (
            ("gate", "wrong", "source_proof_identity_mismatch"),
            ("provider", "wrong", "source_proof_identity_mismatch"),
            ("permission", "wrong", "source_proof_identity_mismatch"),
            ("mode", "wrong", "source_proof_identity_mismatch"),
            ("scenario", "wrong", "source_proof_identity_mismatch"),
            ("requested_options_state", "missing", "source_requested_options_missing"),
            ("requested_options_state", "invalid", "source_requested_options_invalid"),
            ("state", "unresolved", "source_proof_incomplete"),
        )
        for field, value, expected in mutations:
            with self.subTest(field=field, value=value):
                proof = {**self.proof, field: value}
                self.assertEqual(contract._source_proof_error(self.row, {}, proof, None), expected)

    def test_first_observed_error_wins(self):
        self.assertEqual(
            contract._source_proof_error(self.row, None, {}, "earlier-error"), "earlier-error",
        )
        self.assertEqual(
            contract._source_proof_error(self.row, None, {}, None), "source_receipt_missing",
        )


if __name__ == "__main__":
    unittest.main()
