"""Cross-tag publication and proof-retention constraints for release workflows."""

from pathlib import Path
import os
import re
import sys
import textwrap
import unittest

from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.test_published_install import PublishedBase, REVISION, TAG


WORKFLOWS = Path(__file__).resolve().parent.parent / ".github/workflows"


def workflow_step(source: str, name: str) -> str:
    return source.split(f"      - name: {name}\n", 1)[1].split("\n      - name: ", 1)[0]


class ReleaseWorkflowContractTests(unittest.TestCase):
    def test_published_recovery_skips_publication_and_keeps_consumer_dependencies(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        self.assertIn("      resume_published:\n", source)
        for name in ("Validate exact proof and public installation prerequisites",
                     "Configure npm credentials", "Publish or verify npm package",
                     "Publish GitHub Release"):
            with self.subTest(name=name):
                self.assertIn("if: ${{ !inputs.resume_published }}", workflow_step(source, name))
        self.assertIn("if: ${{ inputs.resume_published }}",
                      workflow_step(source, "Validate already published release"))
        prerequisites = workflow_step(source, "Validate release prerequisites")
        self.assertNotIn("inputs.resume_published", prerequisites)
        self.assertNotIn("NPM_TOKEN", prerequisites)
        for check in ("package.json", "git merge-base --is-ancestor", "push_conclusion", "checksums.txt"):
            self.assertIn(check, prerequisites)
        self.assertIn("    needs: promote\n", source)
        self.assertIn("    needs: published-npm\n", source)
        self.assertIn("    needs: publish-homebrew\n", source)

    def test_every_tag_uses_one_workflow_wide_publication_lock(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        concurrency = source.split("\nconcurrency:\n", 1)[1].split("\njobs:\n", 1)[0]
        group = re.search(r"^  group: ([^\n]+)$", concurrency, re.MULTILINE)
        self.assertIsNotNone(group)
        if group is None:
            raise AssertionError("workflow-wide publication lock missing")
        self.assertEqual(group.group(1), "release-promotion")
        self.assertNotIn("${{", group.group(1))
        self.assertIn("  queue: max", concurrency)
        self.assertIn("  cancel-in-progress: false", concurrency)
        self.assertNotRegex(source.split("\njobs:\n", 1)[1], r"(?m)^ +concurrency:")

    def test_native_reports_have_a_documented_public_promotion_window(self):
        source = (WORKFLOWS / "release-native-acceptance.yml").read_text(encoding="utf-8")
        self.assertIn("retention-days: 90", source)
        documentation = (WORKFLOWS.parent.parent / "docs/contributing.md").read_text(encoding="utf-8")
        self.assertIn("Attempt reports request 90-day retention", documentation)
        self.assertIn("all tags", documentation)


class ConsumerReceiptIntegrationTests(PublishedBase):
    def test_recovery_guard_requires_latest_public_stable_release(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        code = textwrap.dedent(workflow_step(
            source, "Validate already published release").split("run: |\n", 1)[1])
        (self.fake_bin / "gh").write_text(
            '#!/bin/sh\ncase "$*" in\n'
            '  *"/releases/latest"*) printf "%s\\n" "$LATEST_RELEASE" ;;\n'
            '  *"/releases/tags/"*) printf "%s\\n" "$PUBLISHED_READY" ;;\n'
            '  *) printf "%s\\n" "$REPOSITORY_PRIVATE" ;;\nesac\n')
        for changes, expected in (({}, 0), ({"REPOSITORY_PRIVATE": "true"}, 1),
                                  ({"PUBLISHED_READY": "false"}, 1),
                                  ({"LATEST_RELEASE": "v9.9.9"}, 1)):
            with self.subTest(changes=changes):
                env = {"PATH": str(self.fake_bin) + os.pathsep + os.defpath,
                       "GITHUB_REPOSITORY": "owner/repository", "TAG": TAG,
                       "LATEST_RELEASE": TAG, "PUBLISHED_READY": "true",
                       "REPOSITORY_PRIVATE": "false", **changes}
                result = run_bounded_command(["/bin/bash", "-euo", "pipefail", "-c", code], env,
                                             max_output=4096, capture=True, timeout=10)
                self.assertEqual(result.exit_code, expected, result.output.decode())

    def workflow_python(self, name: str) -> None:
        source = (WORKFLOWS / "release-consumer.yml").read_text(encoding="utf-8")
        step = workflow_step(source, name)
        code = step.split("python3 - <<'PY'\n", 1)[1].split("\n          PY", 1)[0]
        env = {"TAG": TAG, "CHANNEL": self.channel, "GITHUB_SHA": REVISION,
               "RUNNER_TEMP": str(self.runner_temp), "GITHUB_OUTPUT": str(self.root / "outputs")}
        result = run_bounded_command([sys.executable, "-c", textwrap.dedent(code)], env,
                                     max_output=4096, capture=True, timeout=10)
        self.assertEqual(result.exit_code, 0, result.output.decode())

    def initialize_workflow(self) -> None:
        self.receipt_path = self.runner_temp / "published-install-receipt.json"
        self.workflow_python("Initialize partial-publication failure receipt before setup")
        self.assertFalse(self.receipt_path.exists())

    def selected_report(self) -> Path:
        self.workflow_python("Select consumer report without replacing verifier state")
        value = (self.root / "outputs").read_text().strip()
        self.assertTrue(value.startswith("path="))
        return Path(value.removeprefix("path="))

    def test_workflow_bootstrap_allows_real_consumer_verification(self):
        self.initialize_workflow()
        self.assertEqual(self.publish(), 0)
        before = self.receipt_path.read_bytes()
        self.assertEqual(self.selected_report(), self.receipt_path)
        self.assertEqual(self.receipt_path.read_bytes(), before)
        self.assertEqual(self.receipt()["verdict"], "pass")

    def test_setup_failure_retains_fallback_without_creating_verifier_state(self):
        self.initialize_workflow()
        report = self.selected_report()
        self.assertEqual(report.name, "published-install-setup-failure.json")
        self.assertIn("published_verification_incomplete", report.read_text())
        self.assertFalse(self.receipt_path.exists())

    def test_verifier_failure_is_preserved_instead_of_setup_fallback(self):
        self.initialize_workflow()
        self.world.view_overrides = {"dist.integrity": "invalid"}
        self.assertEqual(self.publish(), 1)
        self.assertEqual(self.selected_report(), self.receipt_path)
        self.assertEqual(self.receipt()["verdict"], "fail")


if __name__ == "__main__":
    unittest.main()
