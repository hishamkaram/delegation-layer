"""SSH acceptance is a required full-scope proof with allowlisted evidence."""

import re
from pathlib import Path
import unittest

from scripts.acceptance.remote.cli import DOCKER_CONTEXT


class RemoteSSHWorkflowTests(unittest.TestCase):
    def setUp(self):
        workflow = Path(__file__).resolve().parents[4] / ".github" / "workflows" / "ci.yml"
        matched = re.search(r"^  remote-ssh:\n(.*?)(?=^  [a-z][a-z-]*:\n|\Z)",
                            workflow.read_text(), re.S | re.M)
        if matched is None:
            raise AssertionError("SSH acceptance job is missing")
        self.job = matched.group(1)
        self.steps = {block.split("\n", 1)[0]: block
                      for block in self.job.split("\n      - name: ")[1:]}

    def test_full_scope_runs_native_linux_without_guard_overrides(self):
        header = self.job.split("    steps:", 1)[0]
        self.assertIn("needs: changes", header)
        self.assertIn("if: ${{ needs.changes.outputs.full == 'true' }}", header)
        self.assertIn("runs-on: ubuntu-24.04", header)
        proof = self.steps["Prove source-built remote SSH behavior"]
        self.assertIn("run: make acceptance-remote-prepared", proof)
        self.assertNotIn("DELEGATE_TOOL_DOWNLOAD_TOKEN", proof)
        install = self.steps["Install pinned SSH supervisor"]
        self.assertIn("DELEGATE_TOOL_DOWNLOAD_TOKEN: ${{ github.token }}", install)
        self.assertIn("run: ./scripts/install-test-supervisor.sh", install)
        self.assertNotIn("continue-on-error", self.job)
        self.assertNotIn("guard=false", self.job)
        self.assertIn("go-version-file: '.go-version'", self.job)

    def test_upload_always_retains_only_sanitized_allowlist(self):
        upload = self.steps["Retain allowlisted SSH acceptance evidence"]
        self.assertIn("if: always()", upload)
        paths = re.findall(r"^            (bin/[^\n]+)$", upload, re.M)
        self.assertEqual(paths, [
            "bin/remote-acceptance/run.*/evidence/receipt.json",
            "bin/remote-acceptance/run.*/evidence/control-evidence-*.json",
        ])
        self.assertIn("if-no-files-found: error", upload)

    def test_prepared_target_tools_are_installed_before_proof(self):
        install = self.steps["Install pinned SSH Go tools"]
        self.assertIn("run: make tools-go", install)
        self.assertIn("DELEGATE_TOOL_DOWNLOAD_TOKEN: ${{ github.token }}", install)
        self.assertLess(self.job.index("- name: Install pinned SSH Go tools"),
                        self.job.index("- name: Prove source-built remote SSH behavior"))
        makefile = Path(__file__).resolve().parents[4] / "Makefile"
        self.assertIn("acceptance-remote-prepared: tool-versions\n", makefile.read_text())

    def test_driver_build_context_exists_and_pins_image_digest(self):
        self.assertEqual(DOCKER_CONTEXT.name, "remote")
        dockerfile = (DOCKER_CONTEXT / "Dockerfile").read_text()
        self.assertRegex(dockerfile.splitlines()[0],
                         r"^FROM debian:bookworm-slim@sha256:[a-f0-9]{64}$")


if __name__ == "__main__":
    unittest.main()
