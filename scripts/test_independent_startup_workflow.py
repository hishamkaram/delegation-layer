"""A failing component proof must remain red without implying full CI success."""

import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest

from scripts.test_candidate_build_workflow import job_text, steps, WORKFLOW


class IndependentStartupWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.source = WORKFLOW.read_text()
        self.job = job_text(self.source, "independent-startup-component")
        self.proof = steps(self.job)["Prove native infrastructure components only"]

    def test_component_mode_is_explicit_and_defaults_to_full_ci(self):
        self.assertRegex(
            self.source,
            r"native_startup_component_only:\n(?:        .*\n)*?"
            r"        default: false\n        type: boolean\n")
        self.assertIn("Native infrastructure component proof only", self.source)
        for name in ("skill-package-linux", "skill-package-macos", "gate-linux",
                     "gate-macos", "nested-survival", "development-candidate",
                     "development-candidate-install", "ccr"):
            with self.subTest(job=name):
                condition = job_text(self.source, name).split("    runs-on:", 1)[0]
                self.assertIn("!inputs.native_startup_component_only", condition)
        self.assertIn("needs.changes.outputs.full == 'true'", self.job)
        self.assertIn("os: [ubuntu-24.04, macos-15]", self.job)
        self.assertIn("DELEGATE_INDEPENDENT_EXPECTED_BACKEND: process-group", self.proof)
        self.assertNotIn("continue-on-error", self.job)

    def test_failed_make_is_not_hidden_by_successful_tee(self):
        shell = re.search(r"^        shell: (\S+)$", self.proof, re.M)
        self.assertIsNotNone(shell, "explicit bash is required for GitHub pipefail")
        self.assertEqual(shell.group(1), "bash")
        script = self.proof.split("        run: |\n", 1)[1]
        script = "\n".join(line.removeprefix("          ") for line in script.splitlines())
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            stub = root / "make"
            stub.write_text("#!/bin/sh\nexit 17\n")
            stub.chmod(0o700)
            result = subprocess.run(
                ["bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", script],
                cwd=root, env={"PATH": str(root) + os.pathsep + os.defpath},
                capture_output=True, text=True, timeout=10, check=False)
            self.assertEqual(result.returncode, 17, result.stderr)
            self.assertTrue((root / "bin/independent-startup-proof/native.log").is_file())

    def test_make_target_runs_required_native_cases_with_race_and_verified_pair(self):
        source = (WORKFLOW.parents[2] / "Makefile").read_text()
        target = source.split("acceptance-independent-startup: tool-versions\n", 1)[1]
        target = target.split("\n\n", 1)[0]
        self.assertIn("./scripts/install-test-supervisor.sh", target)
        self.assertIn("DELEGATE_INDEPENDENT_EXPECTED_BACKEND:?", target)
        self.assertIn("go test -race -count=1 -timeout=4m", target)
        self.assertIn("NativeProof|PeerProof|CustodyNativeProof|OwnStatePairProof", target)
        for package in ("./internal/lifetime", "./internal/scheduler/host", "./internal/supervisor"):
            self.assertIn(package, target)
        for package, cases in (
            ("internal/lifetime", ("TestIndependentStartupNativeProof",)),
            ("internal/scheduler/host", ("TestIndependentStartupPeerProof",)),
            ("internal/supervisor", ("TestIndependentStartupCustodyNativeProof",
                                     "TestIndependentStartupOwnStatePairProof")),
        ):
            self.assert_native_cases_exist(package, cases)

    def test_native_target_exercises_production_foundations(self):
        source = (WORKFLOW.parents[2] / "Makefile").read_text().replace("\\\n", " ")
        target = source.split("acceptance-independent-startup: tool-versions\n", 1)[1]
        target = target.split("\n\n", 1)[0]
        commands = [shlex.split(line) for line in target.splitlines()
                    if line.strip().startswith("go test ")]
        foundation = next(command for command in commands
                          if "./internal/supervisor/infrastructure" in command)
        for option in ("-race", "-count=1", "-timeout=4m"):
            self.assertIn(option, foundation)
        for package in ("./internal/supervisor", "./internal/taskdir"):
            self.assertIn(package, foundation)
        pattern = foundation[foundation.index("-run") + 1].replace("$$", "$")
        required = {
            "internal/supervisor": (
                "TestInfrastructureRuntimeSourceFreezesManagedBytes",
                "TestInfrastructurePairSourceUsesPublishedBinding",
                "TestValidateInfrastructureArtifactsChecksTargetCustody",
                "TestInfrastructureLaunchContextPreservesSelectedCallbackAndMaintenanceScopes",
                "TestStatePairContentStagingPreservesSelection"),
            "internal/supervisor/infrastructure": (
                "TestLeaseInheritedReferenceRetainsExclusion",
                "TestAdoptionDoesNotAcquireFreeFlock",
                "TestCloseSerializesWithBorrower",
                "TestOriginValidateRootIsObservationalAndIdentityBound",
                "TestReadOriginRefusesMalformedRouting",
                "TestStartupIntentObservationNeverRenewsDeadline",
                "TestOwnerPublicationBindsOriginalAssumption",
                "TestOwnerJournalCreatesAssumptionAndPublicationOnce",
                "TestOwnerJournalAssumptionChecksOriginalDeadlineInTransaction",
                "TestInfrastructureContextStagingDoesNotRequireResidentLease",
                "TestIntentJournalCanonicalPublication"),
            "internal/taskdir": ("TestInfrastructurePassiveMissingJournal",
                                 "TestRootPinRejectsReplacementWithIdenticalMetadata"),
        }
        for package, cases in required.items():
            self.assert_native_cases_exist(package, cases)
            for case in cases:
                with self.subTest(case=case):
                    self.assertRegex(case, pattern)

    def assert_native_cases_exist(self, package, cases):
        directory = WORKFLOW.parents[2] / package
        source = "\n".join(path.read_text() for path in directory.glob("*_test.go"))
        declared = re.findall(r"^func (Test\w+)\(", source, re.M)
        for case in cases:
            with self.subTest(package=package, case=case):
                self.assertIn(case, declared, "native target must name an existing test")


if __name__ == "__main__":
    unittest.main()
