"""Contract tests for the trusted-main development readiness workflow."""

from pathlib import Path
import re
import unittest


WORKFLOW = Path(__file__).resolve().parent.parent / ".github" / "workflows" / "development-readiness.yml"
CHECKOUT_ACTION = "actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09"
PYTHON_ACTION = "actions/setup-python@ece7cb06caefa5fff74198d8649806c4678c61a1"
UPLOAD_ACTION = "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02"
FORBIDDEN = ("pull_request_target", "workflow_run", "download-artifact", "actions/cache",
             "secrets.", "id-token", "contents: write", "pull-requests: write", "administration",
             "github.event.pull_request", "head_ref", "refs/pull", "rulesets", "git push", "gh api", "make ",
             "go build", "go run", "npm", "concurrency:")


def steps(source):
    return {block.split("\n", 1)[0]: block for block in source.split("\n      - name: ")[1:]}


class DevelopmentReadinessWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.source = WORKFLOW.read_text()
        self.steps = steps(self.source)

    def test_only_a_manual_dispatch_with_exactly_three_required_inputs_triggers_it(self):
        trigger = self.source.split("\non:\n", 1)[1].split("\npermissions:", 1)[0]
        self.assertEqual(re.findall(r"^  (\w+):", trigger, re.M), ["workflow_dispatch"])
        self.assertEqual(re.findall(r"^      (\w+):", trigger, re.M), ["pull_request", "candidate_id", "freeze_commit"])
        self.assertEqual(trigger.count("required: true"), 3)
        self.assertNotIn("default:", trigger)

    def test_the_job_runs_only_on_main_with_exactly_the_three_proven_scopes(self):
        self.assertIn("\npermissions: {}\n", self.source)
        self.assertEqual(re.findall(r"^  ([a-z][a-z-]*):\n", self.source.split("\njobs:\n", 1)[1], re.M),
                         ["development-readiness"])
        self.assertIn("    if: ${{ github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' }}\n",
                      self.source)
        self.assertIn("    permissions:\n      contents: read\n      pull-requests: read\n      statuses: write\n    env:",
                      self.source)
        for forbidden in FORBIDDEN:
            self.assertNotIn(forbidden, self.source)

    def test_it_executes_only_the_exact_trusted_commit_with_pinned_actions(self):
        checkout = self.steps["Checkout the exact trusted main commit"]
        for setting in (f"uses: {CHECKOUT_ACTION}", "ref: ${{ github.sha }}", "persist-credentials: false"):
            self.assertIn(setting, checkout)
        python = self.steps["Set up Python"]
        self.assertIn(f"uses: {PYTHON_ACTION}", python)
        self.assertIn("python-version: '3.12'", python)
        self.assertEqual(set(re.findall(r"^ +uses: (\S+)$", self.source, re.M)), {CHECKOUT_ACTION, PYTHON_ACTION, UPLOAD_ACTION})

    def test_the_run_binding_and_inputs_are_verified_before_the_observation(self):
        verify = self.steps["Verify the run binding and dispatch inputs"]
        for check in ('"${GITHUB_EVENT_NAME}" = workflow_dispatch', '"${GITHUB_REF}" = refs/heads/main',
                      '.github/workflows/development-readiness.yml@refs/heads/main',
                      '"$(git rev-parse HEAD)" = "${GITHUB_SHA}"', "^[1-9][0-9]{0,9}$", "^[0-9a-f]{64}$",
                      "^[0-9a-f]{40}$", 'mkdir "${RUNNER_TEMP}/development-readiness"'):
            self.assertIn(check, verify)
        self.assertNotIn("${{", verify, "inputs reach the shell only through the environment")
        self.assertLess(self.source.index("Verify the run binding"), self.source.index("check_candidate_readiness.py"))

    def test_the_observation_uses_the_trusted_store_default_and_only_the_workflow_token(self):
        run = self.steps["Observe readiness and post the status on the pull request head"]
        self.assertEqual(re.findall(r"^          (--[a-z-]+) ", run, re.M),
                         ["--pull-request", "--candidate-id", "--freeze-commit", "--output-dir"])
        for argument in ('python3 -B -P scripts/check_candidate_readiness.py', '--pull-request "${PULL_REQUEST}"',
                         '--candidate-id "${CANDIDATE_ID}"', '--freeze-commit "${FREEZE_COMMIT}"',
                         '--output-dir "${RUNNER_TEMP}/development-readiness"'):
            self.assertIn(argument, run)
        for override in ("--store", "STORE_", "inputs.store", "development-proof/evidence"):
            self.assertNotIn(override, self.source, "the store is the CLI's reviewed default, never a run input")
        self.assertEqual(re.findall(r"^          (\w+): ", run.split("env:", 1)[1].split("run:", 1)[0], re.M),
                         ["GH_TOKEN", "PYTHONPATH"])
        self.assertIn("GH_TOKEN: ${{ github.token }}", run)
        self.assertEqual(self.source.count("${{ inputs."), 3)

    def test_available_receipt_evidence_is_always_retained_immutably(self):
        upload = self.steps["Retain the readiness receipt and status request bodies"]
        for setting in ("if: always()", f"uses: {UPLOAD_ACTION}", "path: ${{ runner.temp }}/development-readiness/",
                        "overwrite: false", "if-no-files-found: error",
                        "name: development-readiness-${{ github.run_id }}-${{ github.run_attempt }}"):
            self.assertIn(setting, upload)


if __name__ == "__main__":
    unittest.main()
