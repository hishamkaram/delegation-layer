"""Contract tests for the manual development-candidate jobs in the CI workflow."""

from pathlib import Path
import re
import unittest


WORKFLOW = Path(__file__).resolve().parent.parent / ".github" / "workflows" / "ci.yml"
UPLOAD_ACTION = "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02"
EXISTING_JOBS = ("skill-package", "gate-linux", "gate-macos")
SETUP_ACTIONS = ("actions/checkout@fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09",
                 "actions/setup-python@ece7cb06caefa5fff74198d8649806c4678c61a1",
                 "actions/setup-node@49933ea5288caeca8642d1e84afbd3f7d6820020")
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
        for name in ("build_candidate", "candidate_version", "candidate_base_commit", "prove_candidate_installs"):
            self.assertIn(f"      {name}:\n", dispatch)
        for name in ("build_candidate", "prove_candidate_installs"):
            self.assertRegex(dispatch, rf"{name}:\n(?:        .*\n)*?        default: false\n(?:        .*\n)*?        type: boolean\n")
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


class CandidateInstallWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.source = WORKFLOW.read_text()
        self.producer = job_text(self.source, "development-candidate")
        self.job = job_text(self.source, "development-candidate-install")
        self.steps = steps(self.job)

    def test_producer_publishes_its_generated_locator_as_the_only_job_output(self):
        self.assertIn("    outputs:\n      candidate-locator: ${{ steps.locator.outputs.locator }}\n", self.producer)
        encode = steps(self.producer)["Encode the external candidate locator"]
        self.assertIn("id: locator\n", encode)
        written = "${RUNNER_TEMP}/candidate-locator/candidate-locator.json"
        self.assertIn(f'printf \'locator=%s\\n\' "$(cat "{written}")" >> "${{GITHUB_OUTPUT}}"', encode)
        self.assertLess(encode.index("build_development_candidate.py locator"), encode.index("GITHUB_OUTPUT"))
        self.assertEqual(self.producer.count("outputs:"), 1)

    def test_no_gate_depends_on_the_candidate_jobs(self):
        for name in (*EXISTING_JOBS, "development-candidate"):
            with self.subTest(job=name):
                self.assertNotIn("needs:", job_text(self.source, name))

    def test_job_runs_only_after_the_build_and_on_both_explicit_opt_ins(self):
        self.assertIn("    needs: development-candidate\n", self.job)
        condition = " ".join(self.job.split("    runs-on:", 1)[0].split("    if:", 1)[1].split())
        for requirement in ("github.event_name == 'workflow_dispatch'", "inputs.build_candidate",
                            "inputs.prove_candidate_installs", "github.ref_type == 'branch'",
                            "github.ref != 'refs/heads/main'"):
            self.assertIn(requirement, condition)
        self.assertNotIn("always()", condition)

    def test_matrix_is_the_two_hosts_by_three_channels_and_fails_independently(self):
        strategy = self.job.split("    strategy:\n", 1)[1].split("    steps:", 1)[0]
        self.assertEqual(strategy, (
            "      fail-fast: false\n"
            "      matrix:\n"
            "        os: [ubuntu-24.04, macos-15]\n"
            "        channel: [shell, npm, homebrew]\n"
            "        include:\n"
            "          - os: ubuntu-24.04\n            host_os: Linux\n            host_arch: amd64\n"
            "          - os: macos-15\n            host_os: Darwin\n            host_arch: arm64\n"))
        self.assertIn("    runs-on: ${{ matrix.os }}\n", self.job)

    def test_job_is_read_only_with_the_token_scoped_to_the_proof_step(self):
        self.assertIn("    permissions:\n      contents: read\n      actions: read\n    strategy:", self.job)
        for forbidden in FORBIDDEN:
            if forbidden != "GITHUB_TOKEN":
                self.assertNotIn(forbidden, self.job)
        self.assertNotIn("GITHUB_TOKEN", self.job)
        proof = self.steps["Prove the candidate install through the channel"]
        self.assertEqual(self.job.count("github.token"), 1)
        self.assertEqual(self.job.count("GH_TOKEN"), 1)
        self.assertIn("          GH_TOKEN: ${{ github.token }}\n", proof)

    def test_checkout_is_the_built_source_commit_without_credentials(self):
        checkout = self.steps["Checkout the source commit the candidate was built from"]
        for setting in ("ref: ${{ github.sha }}", "fetch-depth: 0", "persist-credentials: false"):
            self.assertIn(setting, checkout)
        for action in SETUP_ACTIONS:
            self.assertIn(f"uses: {action}\n", self.job)
        self.assertIn("python-version: '3.12'", self.job)
        self.assertIn("node-version: '22.x'", self.job)
        for tool in ("setup-go", "make tools"):
            self.assertNotIn(tool, self.job)

    def test_locator_is_the_producers_bytes_never_rebuilt_or_refetched(self):
        write = self.steps["Write the producer's candidate locator"]
        self.assertIn("CANDIDATE_LOCATOR: ${{ needs.development-candidate.outputs.candidate-locator }}", write)
        self.assertIn('if [ -z "${CANDIDATE_LOCATOR}" ]; then', write)
        self.assertIn("exit 1", write)
        self.assertIn("printf '%s\\n' \"${CANDIDATE_LOCATOR}\"", write)
        for rebuilt in ("build_development_candidate", "github.run_id", "github.run_attempt",
                        "artifact-id", "download-artifact"):
            self.assertNotIn(rebuilt, write)
        proof = self.steps["Prove the candidate install through the channel"]
        for argument in ('--locator "${RUNNER_TEMP}/candidate-install-input/candidate-locator.json"',
                         '--channel "${CHANNEL}"', '--output-dir "${RUNNER_TEMP}/candidate-install-proof"',
                         "--github-auth environment", "python3 scripts/prove_candidate_install.py"):
            self.assertIn(argument, proof)
        self.assertNotIn("--bootstrap-parent", proof)
        self.assertEqual(re.findall(r"^          (\w+): ", proof.split("env:", 1)[1].split("run:", 1)[0], re.M),
                         ["GH_TOKEN", "CHANNEL"])

    def test_native_platform_is_checked_before_any_install_work(self):
        names = list(self.steps)
        check = self.steps["Require the intended native platform"]
        self.assertIn("EXPECTED_OS: ${{ matrix.host_os }}", check)
        self.assertIn("EXPECTED_ARCH: ${{ matrix.host_arch }}", check)
        self.assertIn("import platform_identity", check)
        self.assertLess(names.index("Require the intended native platform"),
                        names.index("Write the producer's candidate locator"))
        self.assertLess(names.index("Write the producer's candidate locator"),
                        names.index("Prove the candidate install through the channel"))

    def test_only_the_linux_homebrew_leg_exposes_the_preinstalled_brew(self):
        expose = self.steps["Expose the preinstalled Linux Homebrew"]
        self.assertIn("if: ${{ matrix.host_os == 'Linux' && matrix.channel == 'homebrew' }}", expose)
        self.assertIn("/home/linuxbrew/.linuxbrew/bin", expose)
        self.assertIn('test -x "${brew_bin}/brew"', expose)
        self.assertEqual(self.job.count("GITHUB_PATH"), 1)
        names = list(self.steps)
        self.assertLess(names.index("Expose the preinstalled Linux Homebrew"),
                        names.index("Prove the candidate install through the channel"))

    def test_only_the_three_evidence_files_are_uploaded_under_a_unique_name(self):
        uploads = [name for name, block in self.steps.items() if "upload-artifact" in block]
        self.assertEqual(uploads, ["Retain the install proof evidence"])
        upload = self.steps[uploads[0]]
        self.assertIn(f"uses: {UPLOAD_ACTION}\n", upload)
        self.assertIn("if: always()", upload)
        self.assertIn("name: candidate-install-${{ matrix.os }}-${{ matrix.channel }}-"
                      "${{ github.run_id }}-${{ github.run_attempt }}\n", upload)
        paths = upload.split("path: |\n", 1)[1].split("          if-no-files-found", 1)[0].split("\n")
        self.assertEqual([path.strip() for path in paths if path.strip()],
                         [f"${{{{ runner.temp }}}}/candidate-install-proof/{name}"
                          for name in ("receipt.json", "install-proof.json", "install-witness.json")])
        for setting in ("overwrite: false", "retention-days: 90"):
            self.assertIn(setting, upload)


if __name__ == "__main__":
    unittest.main()
