"""The release workflow's dispatch, run title, permissions, credentials and report artifact match the shared protocol."""

from __future__ import annotations

from pathlib import Path
import re
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

PUBLICATION_WORKFLOW = "release.yml"
INPUT_NAMES = ("artifact_id", "resume_published")
from scripts.delegate_acceptance.release_verification.candidate_publication_delivery import (
    RECEIPT_NAME,
)
from scripts.test_release_workflow_contracts import (
    PUBLICATION_JOB, PUBLISH_STEP, RECEIPT_STEP, WORKFLOWS, conditions, job_steps, workflow_job, workflow_step,
)


SOURCE = (WORKFLOWS / PUBLICATION_WORKFLOW).read_text(encoding="utf-8")
# Every third-party action is pinned to a full commit; these are the existing pinned versions.
PINNED_ACTIONS = {
    "actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
    "actions/setup-python@ece7cb06caefa5fff74198d8649806c4678c61a1",
    "actions/setup-node@49933ea5288caeca8642d1e84afbd3f7d6820020",
    "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
}
JOB_PERMISSIONS = {"actions": "read", "contents": "write"}
NPM_TOKEN_VARIABLE = "NPM_TOKEN"
TAP_TOKEN_VARIABLE = "HOMEBREW_TAP_TOKEN"
OUTPUT_DIR = "distribution-publication"


def inputs_block() -> str:
    return SOURCE.split("\n    inputs:\n", 1)[1].split("\n\npermissions:", 1)[0]


def mapping(block: str, indent: int) -> dict[str, str]:
    return dict(re.findall(rf"(?m)^ {{{indent}}}([A-Za-z0-9_-]+): (.+)$", block))


class DispatchContractTests(unittest.TestCase):
    def test_the_inputs_are_exactly_artifact_id_and_explicit_resume(self):
        block = inputs_block()
        names = re.findall(r"(?m)^      ([a-z_0-9]+):$", block)
        self.assertEqual(tuple(names), INPUT_NAMES)
        for name, body in zip(names, re.split(r"(?m)^      [a-z_0-9]+:\n", block)[1:]):
            with self.subTest(name=name):
                fields = mapping(body, 8)
                self.assertEqual(fields["required"], "true")
                self.assertEqual(fields["type"], "string" if name == "artifact_id" else "boolean")
                if name == "resume_published":
                    self.assertEqual(fields["default"], "false")
                else:
                    self.assertNotIn("default", fields)

    def test_the_cli_receives_every_input_by_its_upper_cased_name(self):
        env = mapping(workflow_step(SOURCE, PUBLISH_STEP).split("        env:\n", 1)[1], 10)
        for name in INPUT_NAMES:
            with self.subTest(name=name):
                self.assertEqual(env[name.upper()], f"${{{{ inputs.{name} }}}}")


class TrustAndCredentialTests(unittest.TestCase):
    def test_only_the_job_holds_the_needed_permissions_and_no_attestation_or_oidc(self):
        top = SOURCE.split("\npermissions:", 1)[1].split("\n", 1)[0].strip()
        self.assertEqual(top, "{}")
        job = workflow_job(SOURCE, PUBLICATION_JOB)
        block = job.split("\n    permissions:\n", 1)[1].split("\n    steps:\n", 1)[0]
        self.assertEqual(mapping(block, 6), JOB_PERMISSIONS)
        for denied in ("id-token", "attestations", "packages"):
            self.assertNotIn(denied, SOURCE)

    def test_the_checkout_is_exactly_h_without_persisted_credentials(self):
        steps = job_steps(workflow_job(SOURCE, PUBLICATION_JOB))
        checkout = steps["Checkout trusted publication code"]
        self.assertEqual(mapping(checkout, 10), {"ref": "${{ github.sha }}", "fetch-depth": "0",
                                                  "persist-credentials": "false"})
        self.assertEqual(SOURCE.count("actions/checkout@"), 1)
        self.assertEqual(set(re.findall(r"(?m)^ +uses: (\S+)$", SOURCE)), PINNED_ACTIONS)
        self.assertIn("python-version: '3.12'", SOURCE)
        self.assertIn("node-version: '22.x'", SOURCE)

    def test_credentials_reach_only_the_publication_step(self):
        steps = job_steps(workflow_job(SOURCE, PUBLICATION_JOB))
        env = mapping(steps[PUBLISH_STEP].split("        env:\n", 1)[1], 10)
        self.assertEqual({key: env[key] for key in ("GH_TOKEN", TAP_TOKEN_VARIABLE, NPM_TOKEN_VARIABLE)},
                         {"GH_TOKEN": "${{ secrets.GITHUB_TOKEN }}",
                          TAP_TOKEN_VARIABLE: "${{ secrets.HOMEBREW_TAP_TOKEN }}",
                          NPM_TOKEN_VARIABLE: "${{ secrets.NPM_TOKEN }}"})
        for name, step in steps.items():
            if name != PUBLISH_STEP:
                with self.subTest(step=name):
                    self.assertNotIn("secrets.", step)
                    self.assertNotIn("github.token", step)
        self.assertEqual(SOURCE.count("secrets."), 3)


class ReportArtifactTests(unittest.TestCase):
    def test_exactly_one_flat_receipt_is_uploaded_under_the_shared_name_always(self):
        step = workflow_step(SOURCE, RECEIPT_STEP)
        self.assertEqual([condition.strip() for condition in conditions(step, 8)], ["always()"])
        self.assertNotIn("continue-on-error", step)
        self.assertEqual(mapping(step, 10), {
            "name": "distribution-publication-${{ github.run_id }}-${{ github.run_attempt }}",
            "path": f"${{{{ runner.temp }}}}/{OUTPUT_DIR}/{RECEIPT_NAME}",
            "if-no-files-found": "ignore", "retention-days": "90"})
        self.assertIn(f'--output-dir "${{RUNNER_TEMP}}/{OUTPUT_DIR}"', workflow_step(SOURCE, PUBLISH_STEP))


if __name__ == "__main__":
    unittest.main()
