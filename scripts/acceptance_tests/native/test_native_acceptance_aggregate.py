"""Hermetic oracles for a native acceptance behavior cohort."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import native_acceptance_evidence as native_evidence
from scripts import acceptance_native as entry
from scripts.acceptance_tests.native.support import (
    AGGREGATE_WRAPPER_TEST_TIMEOUT,
    fake_native_python_script,
    physical_test_root,
    repository_root,
)
from scripts.delegate_acceptance.native import (
    acceptance as gate,
)


class NativeAcceptanceOracleTests(unittest.TestCase):
    def test_profiles_and_parser_expose_normal_permission_matrix(self):
        expected = {"antigravity:print", "ccr:print", "claude:print", "codex:exec", "codex:review", "pi:json", "opencode:run"}
        self.assertEqual(set(gate.PROFILES), expected)
        self.assertEqual(gate.PROFILES["antigravity:print"]["default_mode"], "workspace-write")
        for profile in gate.PROFILES.values():
            if profile is not gate.PROFILES["codex:review"]:
                self.assertIn("workspace-write", profile["modes"])
        self.assertEqual(gate.PROFILES["claude:print"]["default_mode"], "read-only")
        self.assertEqual(gate.PROFILES["codex:exec"]["default_mode"], "read-only")
        self.assertEqual(gate.PROFILES["codex:review"]["modes"], ("read-only",))
        self.assertEqual(gate.PROFILES["pi:json"]["default_mode"], "read-only")
        self.assertEqual(gate.PROFILES["opencode:run"]["default_mode"], "read-only")

        write_args = entry.build_parser().parse_args([
            "--provider", "codex:exec", "--permission", "workspace-write", "--scenario", "write",
        ])
        self.assertEqual(write_args.mode, "workspace-write")
        self.assertEqual(write_args.scenario, "write")
        legacy_args = entry.build_parser().parse_args([
            "--provider", "pi:json", "--mode", "workspace-write",
        ])
        self.assertEqual(legacy_args.mode, "workspace-write")
        self.assertIsNone(legacy_args.scenario)
        discovery_args = entry.build_parser().parse_args([
            "--provider", "antigravity:print", "--permission", "read-only",
            "--scenario", "read-only", "--discover-models",
        ])
        self.assertTrue(discovery_args.discover_models)
        with self.assertRaises(SystemExit):
            entry.build_parser().parse_args([
                "--provider", "pi:json", "--permission", "workspace-write", "--scenario", "timeout",
            ])

    def test_public_option_docs_cover_ccr_required_model_and_effort(self):
        repository = repository_root()
        cli_reference = (repository / "docs/cli-reference.md").read_text(encoding="utf-8")
        troubleshooting = (repository / "docs/troubleshooting.md").read_text(encoding="utf-8")
        self.assertIn("Codex, CCR, Claude, AGY, Pi, or OpenCode", cli_reference)
        self.assertIn("`ccr:print` requires a configured CCR alias", cli_reference)
        self.assertIn("CCR, Claude, and AGY pass their native effort flag", cli_reference)
        self.assertIn("`codex:exec`, CCR, and Claude support both modes plus `model` and `effort`", troubleshooting)
        self.assertIn("`codex:review` is read-only and requires a review target.", troubleshooting)
        self.assertIn("`ccr:print`\nrequires `--model` to be a configured CCR alias", troubleshooting)
        self.assertIn("CCR, Claude, and AGY pass native effort", troubleshooting)

    def test_aggregate_wrapper_runs_write_and_read_profiles(self):
        wrapper = (repository_root() / "scripts/acceptance_native_all.sh").read_text()
        self.assertIn("list-cases", wrapper)
        self.assertIn('done <"$case_plan"', wrapper)
        self.assertIn('--runner "$artifact_bin/delegate-run" </dev/null', wrapper)
        self.assertIn('--permission "$case_permission"', wrapper)
        self.assertIn('--scenario "$case_scenario"', wrapper)
        self.assertIn('if [ "$case_provider" = opencode:run ] && [ -n "${OPENCODE_LIVE_MODEL:-}" ]; then', wrapper)
        self.assertIn('set -- "$@" --model "$OPENCODE_LIVE_MODEL"', wrapper)
        individual = (repository_root() / "scripts/acceptance_native.sh").read_text()
        self.assertIn('if [ "$provider" = opencode:run ] && [ -n "${OPENCODE_LIVE_MODEL:-}" ]; then', individual)
        self.assertIn('set -- --model "$OPENCODE_LIVE_MODEL" "$@"', individual)
        self.assertNotIn('timeout-process.json', wrapper)
        self.assertIn("2) # User-authorized authentication/prerequisite blocks remain neutral.", wrapper)
        self.assertIn("passed=%s blocked=%s failed=%s", wrapper)
        self.assertIn("aggregate_status=BLOCKED", wrapper)

    def test_aggregate_reports_counts_and_neutral_all_blocked_status(self):
        wrapper = repository_root() / "scripts/acceptance_native_all.sh"
        with tempfile.TemporaryDirectory(prefix="native-aggregate-unit-") as directory:
            fake_python = Path(directory) / "python3"
            fake_python.write_text(fake_native_python_script())
            fake_python.chmod(0o700)
            environment = os.environ.copy()
            environment["PATH"] = str(fake_python.parent) + os.pathsep + environment["PATH"]
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(
                physical_test_root(directory) / "aggregate")
            environment["REAL_PYTHON"] = sys.executable
            environment["FAKE_NATIVE_RESULT"] = "blocked"
            environment.pop("CCR_LIVE_MODEL", None)
            environment.pop("DELEGATION_REQUIRE_LIVE_ACCEPTANCE", None)
            blocked = subprocess.run(
                ["sh", str(wrapper)], cwd=repository_root(), env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)
            self.assertEqual(blocked.returncode, 0)
            self.assertIn(f"status=BLOCKED passed=0 blocked={len(native_evidence.EXPECTED_CASES)} failed=0", blocked.stdout)
            self.assertIn("auth_blocks_neutral=true", blocked.stdout)
            aggregate = json.loads((Path(environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"])
                                    / "aggregate-receipt.json").read_text())
            ccr_cases = [case for case in aggregate["cases"] if case["provider"] == "ccr:print"]
            self.assertEqual(len(ccr_cases), 2)
            for case in ccr_cases:
                compact = json.loads(Path(case["receipt_path"]).read_text())
                self.assertEqual(compact["source_proof"]["requested_options_state"], "recorded")
                self.assertIsNone(compact["source_proof"]["requested_model"])

            environment["DELEGATION_REQUIRE_LIVE_ACCEPTANCE"] = "1"
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(
                physical_test_root(directory) / "aggregate-required")
            required = subprocess.run(
                ["sh", str(wrapper)], cwd=repository_root(), env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)
            self.assertEqual(required.returncode, 2)
            self.assertIn(f"status=BLOCKED passed=0 blocked={len(native_evidence.EXPECTED_CASES)} failed=0", required.stdout)
            environment.pop("DELEGATION_REQUIRE_LIVE_ACCEPTANCE")

            fake_python.write_text(fake_native_python_script())
            environment["FAKE_NATIVE_RESULT"] = "mixed"
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(
                physical_test_root(directory) / "aggregate-mixed")
            mixed = subprocess.run(
                ["sh", str(wrapper)], cwd=repository_root(), env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)
            self.assertEqual(mixed.returncode, 1)
            self.assertIn(f"status=failed passed=2 blocked=9 failed={sum(case.provider == 'codex:exec' for case in native_evidence.EXPECTED_CASES)}", mixed.stdout)

            fake_python.write_text(fake_native_python_script())
            environment["FAKE_NATIVE_RESULT"] = "timeout"
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(
                physical_test_root(directory) / "aggregate-timeout")
            successful_timeout = subprocess.run(
                ["sh", str(wrapper)], cwd=repository_root(), env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)
            self.assertEqual(successful_timeout.returncode, 0)
            self.assertIn(f"status=passed passed=3 blocked={len(native_evidence.EXPECTED_CASES) - 3} failed=0", successful_timeout.stdout)

    def test_aggregate_uses_each_case_exit_contract_without_a_second_receipt_oracle(self):
        wrapper = repository_root() / "scripts/acceptance_native_all.sh"
        with tempfile.TemporaryDirectory(prefix="native-aggregate-timeout-unit-") as directory:
            fake_python = Path(directory) / "python3"
            fake_python.write_text(fake_native_python_script())
            fake_python.chmod(0o700)
            environment = os.environ.copy()
            environment["PATH"] = str(fake_python.parent) + os.pathsep + environment["PATH"]
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(
                physical_test_root(directory) / "aggregate")
            environment["REAL_PYTHON"] = sys.executable
            environment["FAKE_NATIVE_RESULT"] = "only-timeout"

            result = subprocess.run(
                ["sh", str(wrapper)], cwd=repository_root(), env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT)

            self.assertEqual(result.returncode, 0)
            self.assertIn(f"status=passed passed=1 blocked={len(native_evidence.EXPECTED_CASES) - 1} failed=0", result.stdout)

    def test_aggregate_rejects_unexpected_evidence_helper_exit(self):
        wrapper = repository_root() / "scripts/acceptance_native_all.sh"
        with tempfile.TemporaryDirectory(prefix="native-aggregate-helper-exit-unit-") as directory:
            fake_python = Path(directory) / "python3"
            fake_python.write_text(fake_native_python_script())
            fake_python.chmod(0o700)
            environment = os.environ.copy()
            environment["PATH"] = str(fake_python.parent) + os.pathsep + environment["PATH"]
            environment["DELEGATE_ACCEPTANCE_OUTPUT_ROOT"] = str(
                physical_test_root(directory) / "aggregate")
            environment["REAL_PYTHON"] = sys.executable
            environment["FAKE_NATIVE_RESULT"] = "blocked"
            environment["FAKE_HELPER_EXIT"] = "7"
            result = subprocess.run(
                ["sh", str(wrapper)], cwd=repository_root(), env=environment,
                capture_output=True, text=True, check=False,
                timeout=AGGREGATE_WRAPPER_TEST_TIMEOUT,
            )
            self.assertEqual(result.returncode, 1)

    def test_normal_setup_does_not_probe_provider_version_directly(self):
        source = (repository_root() / "scripts/acceptance_native.py").read_text()
        self.assertNotIn('[self.provider_executable, "--version"]', source)
