"""Contract tests for the manual development-candidate job in the CI workflow."""

from pathlib import Path
import re
import unittest


WORKFLOW = Path(__file__).resolve().parent.parent / ".github" / "workflows" / "ci.yml"
UPLOAD_ACTION = "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02"
EXISTING_JOBS = ("skill-package", "gate-linux", "gate-macos")
FORBIDDEN = ("git tag", "git push", "gh release", "gh api", "npm publish", "attest", "id-token",
             "secrets.", "GITHUB_TOKEN", "contents: write", "--snapshot", "--skip=validate")


def job_text(source, name):
    match = re.search(rf"^  {re.escape(name)}:\n(.*?)(?=^  [a-z][a-z-]*:\n|\Z)", source, re.S | re.M)
    if match is None:
        raise AssertionError(f"job {name} is missing")
    return match.group(1)


def steps(job):
    return {block.split("\n", 1)[0]: block for block in job.split("\n      - name: ")[1:]}


class CandidateWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.source = WORKFLOW.read_text()
        self.job = job_text(self.source, "development-candidate")
        self.steps = steps(self.job)

    def test_existing_gate_jobs_remain_unconditional(self):
        for name in EXISTING_JOBS:
            with self.subTest(job=name):
                header = job_text(self.source, name).split("    steps:", 1)[0]
                self.assertNotIn("\n    if:", header)
                self.assertNotIn("candidate", header)
        self.assertIn("runs-on: ubuntu-24.04", job_text(self.source, "gate-linux"))
        self.assertIn("runs-on: macos-15", job_text(self.source, "gate-macos"))

    def test_dispatch_inputs_default_to_no_candidate(self):
        dispatch = self.source.split("  workflow_dispatch:\n", 1)[1].split("\npermissions:", 1)[0]
        for name in ("build_candidate", "candidate_version", "candidate_base_commit"):
            self.assertIn(f"      {name}:\n", dispatch)
        self.assertRegex(dispatch, r"build_candidate:\n(?:        .*\n)*?        default: false\n")
        self.assertIn("  push:\n    branches: [\"main\"]", self.source)

    def test_job_runs_only_on_manual_dispatch_of_a_task_branch(self):
        condition = " ".join(self.job.split("    runs-on:", 1)[0].split())
        for requirement in ("github.event_name == 'workflow_dispatch'", "inputs.build_candidate",
                            "github.ref_type == 'branch'", "github.ref != 'refs/heads/main'"):
            self.assertIn(requirement, condition)
        self.assertNotIn("needs:", self.job)

    def test_job_is_read_only_and_never_publishes_or_tags(self):
        self.assertIn("    permissions:\n      contents: read\n", self.job)
        for forbidden in FORBIDDEN:
            self.assertNotIn(forbidden, self.job)
        checkout = self.steps["Checkout repository"]
        self.assertIn("persist-credentials: false", checkout)
        self.assertIn("fetch-depth: 0", checkout)

    def test_build_step_passes_exact_expectations_and_no_credentials(self):
        build = self.steps["Build the candidate from the clean committed checkout"]
        for argument in ('--version "${CANDIDATE_VERSION}"', '--commit "${GITHUB_SHA}"',
                         '--base-commit "${CANDIDATE_BASE_COMMIT}"', '--repository "${GITHUB_REPOSITORY}"',
                         '--goreleaser "${GITHUB_WORKSPACE}/bin/goreleaser"'):
            self.assertIn(argument, build)
        self.assertIn("scripts/build_development_candidate.py build", build)
        self.assertEqual(re.findall(r"^          (\w+): ", build.split("env:", 1)[1].split("run:", 1)[0], re.M),
                         ["CANDIDATE_VERSION", "CANDIDATE_BASE_COMMIT"])
        self.assertLess(self.job.index("make tools"), self.job.index("make tool-versions"))
        self.assertLess(self.job.index("make tool-versions"), self.job.index("build_development_candidate.py build"))

    def test_primary_artifact_is_immutable_and_holds_only_the_payload(self):
        upload = self.steps["Upload the immutable candidate payload"]
        self.assertIn(f"uses: {UPLOAD_ACTION}", upload)
        for setting in ("overwrite: false", "if-no-files-found: error", "retention-days: 90",
                        "path: ${{ runner.temp }}/candidate-payload/",
                        "name: development-candidate-${{ github.run_id }}-${{ github.run_attempt }}"):
            self.assertIn(setting, upload)

    def test_locator_is_encoded_after_upload_and_retained_outside_the_payload(self):
        names = list(self.steps)
        self.assertLess(names.index("Upload the immutable candidate payload"),
                        names.index("Encode the external candidate locator"))
        encode = self.steps["Encode the external candidate locator"]
        for fragment in ("steps.candidate.outputs.artifact-id", "steps.candidate.outputs.artifact-digest",
                         "candidate-payload/candidate-manifest.json", "candidate-locator/candidate-locator.json"):
            self.assertIn(fragment, encode)
        locator = self.steps["Retain the candidate locator outside the payload"]
        self.assertIn("path: ${{ runner.temp }}/candidate-locator/candidate-locator.json", locator)
        self.assertNotIn("candidate-payload", locator)
        self.assertIn("overwrite: false", locator)

    def test_every_upload_is_pinned_and_none_overwrites(self):
        uploads = [block for block in self.steps.values() if "upload-artifact" in block]
        self.assertEqual(len(uploads), 3)
        for block in uploads:
            self.assertIn(f"uses: {UPLOAD_ACTION}\n", block)
            self.assertIn("overwrite: false", block)
            self.assertIn("retention-days: 90", block)
        receipt = self.steps["Retain the sanitized build receipt"]
        self.assertIn("if: always()", receipt)


if __name__ == "__main__":
    unittest.main()
