"""Distribution jobs preserve the requested source and install their producer's bytes."""

from pathlib import Path
import re
import unittest


WORKFLOW = Path(__file__).resolve().parent.parent / ".github" / "workflows" / "ci.yml"
SOURCE_COMMIT = "${{ github.event.pull_request.head.sha || github.sha }}"
UPLOAD_ACTION = "actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02"
EXISTING_JOBS = ("skill-package-linux", "skill-package-macos", "gate-linux", "gate-macos")
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

    def test_required_jobs_keep_fixed_names_and_native_jobs_select_before_allocation(self):
        names = {"skill-package-linux": "Agent skill package (ubuntu-24.04)",
                 "skill-package-macos": "Agent skill package (macos-15)",
                 "gate-linux": "Gate (Linux ubuntu-24.04)",
                 "gate-macos": "Gate (macOS macos-15)"}
        for job, name in names.items():
            with self.subTest(job=job):
                header = job_text(self.source, job).split("    steps:", 1)[0]
                self.assertIn(f"name: {name}\n", header)
                self.assertNotIn("matrix", header)
                self.assertNotIn("candidate", header)
        for job in ("skill-package-macos", "gate-macos"):
            header = job_text(self.source, job).split("    steps:", 1)[0]
            self.assertIn("needs: changes", header)
            self.assertIn("if: ${{ !inputs.native_startup_component_only && needs.changes.outputs.full == 'true' }}", header)
        self.assertIn("runs-on: ubuntu-24.04", job_text(self.source, "gate-linux"))
        self.assertIn("runs-on: macos-15", job_text(self.source, "gate-macos"))

    def test_dispatch_inputs_default_to_no_candidate(self):
        dispatch = self.source.split("  workflow_dispatch:\n", 1)[1].split("\npermissions:", 1)[0]
        for name in ("build_candidate", "candidate_version", "candidate_base_commit"):
            self.assertIn(f"      {name}:\n", dispatch)
        for name in ("build_candidate",):
            self.assertRegex(dispatch, rf"{name}:\n(?:        .*\n)*?        default: false\n(?:        .*\n)*?        type: boolean\n")
        self.assertIn("  push:\n    branches: [\"main\"]", self.source)

    def test_each_node_version_has_one_package_check_per_host(self):
        for name in ("skill-package-linux", "skill-package-macos"):
            package = job_text(self.source, name)
            self.assertIn("node-version: '18.x'", package)
            self.assertNotIn("node-version: '22.x'", package)
            self.assertEqual(package.count("run: make skill-package-check"), 1)
        for name, tool_target, check_target in (("gate-linux", "tools", "check"),
                                                 ("gate-macos", "tools-go", "check-platform")):
            with self.subTest(job=name):
                gate = job_text(self.source, name)
                self.assertIn("node-version: '22.x'", gate)
                self.assertEqual(re.findall(r"^        run: make (tools(?:-go)?)$", gate, re.M), [tool_target])
                self.assertEqual(re.findall(r"^        run: make (check(?:-platform)?)$", gate, re.M), [check_target])

    def test_shared_analysis_is_required_by_linux_without_a_duplicate_workflow(self):
        linux = job_text(self.source, "gate-linux")
        macos = job_text(self.source, "gate-macos")
        self.assertEqual(linux.count("run: make check\n"), 1)
        self.assertNotIn("continue-on-error", linux)
        self.assertIn("run: make check-platform\n", macos)
        self.assertFalse(WORKFLOW.with_name("static-diagnostics.yml").exists())

    def test_ci_and_ccr_checkouts_do_not_persist_credentials(self):
        for workflow in (WORKFLOW, WORKFLOW.with_name("ccr-integration.yml")):
            with self.subTest(workflow=workflow.name):
                checkouts = [block for block in workflow.read_text().split("\n      - ")
                             if re.search(r"(?m)^(?:        )?uses: actions/checkout@", block)]
                self.assertTrue(checkouts, "workflow must contain a checkout")
                for checkout in checkouts:
                    self.assertRegex(checkout, r"(?m)^          persist-credentials: false$")

    def test_full_pr_or_main_and_explicit_manual_candidate_build_the_distribution(self):
        condition = " ".join(self.job.split("    runs-on:", 1)[0].split())
        for requirement in ("github.event_name == 'workflow_dispatch'", "inputs.build_candidate",
                            "github.ref_type == 'branch'"):
            self.assertIn(requirement, condition)
        self.assertIn("github.event_name == 'pull_request'", condition)
        self.assertIn("github.event.pull_request.head.repo.full_name == github.repository", condition)
        self.assertIn("github.event_name == 'push'", condition)
        self.assertNotIn("github.ref != 'refs/heads/main'", condition)
        self.assertIn("needs: changes", self.job)
        self.assertIn("needs.changes.outputs.full == 'true'", condition)

    def test_job_is_read_only_and_never_publishes_or_tags(self):
        self.assertIn("    permissions:\n      contents: read\n", self.job)
        for forbidden in FORBIDDEN:
            self.assertNotIn(forbidden, self.job)
        checkout = self.steps["Checkout repository"]
        self.assertIn("persist-credentials: false", checkout)
        self.assertIn("fetch-depth: 0", checkout)
        self.assertIn(f"ref: {SOURCE_COMMIT}", checkout)

    def test_build_step_passes_exact_expectations_and_no_credentials(self):
        build = self.steps["Build the candidate from the clean committed checkout"]
        for argument in ('--version "${candidate_version}"', '--commit "${CANDIDATE_COMMIT}"',
                         '--base-commit "${CANDIDATE_BASE_COMMIT:-$CANDIDATE_COMMIT}"', '--repository "${GITHUB_REPOSITORY}"',
                         '--goreleaser "${GITHUB_WORKSPACE}/bin/goreleaser"'):
            self.assertIn(argument, build)
        self.assertIn("scripts/build_development_candidate.py build", build)
        self.assertEqual(re.findall(r"^          (\w+): ", build.split("env:", 1)[1].split("run:", 1)[0], re.M),
                         ["CANDIDATE_VERSION", "CANDIDATE_BASE_COMMIT", "CANDIDATE_COMMIT"])
        self.assertIn(f"CANDIDATE_COMMIT: {SOURCE_COMMIT}", build)
        self.assertLess(self.job.index("make tools"), self.job.index("make tool-versions"))
        self.assertLess(self.job.index("make tool-versions"), self.job.index("build_development_candidate.py build"))

    def test_primary_artifact_is_immutable_and_holds_only_the_payload(self):
        upload = self.steps["Upload the immutable candidate payload"]
        self.assertIn(f"uses: {UPLOAD_ACTION}", upload)
        for setting in ("overwrite: false", "if-no-files-found: error", "retention-days: 90",
                        "path: ${{ runner.temp }}/candidate-payload/",
                        "name: development-candidate-${{ github.run_id }}-${{ github.run_attempt }}"):
            self.assertIn(setting, upload)

    def test_every_upload_is_pinned_and_none_overwrites(self):
        uploads = [block for block in self.steps.values() if "upload-artifact" in block]
        self.assertEqual(len(uploads), 2)
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

    def test_producer_exposes_the_immutable_artifact_id_without_an_external_locator(self):
        self.assertIn("    outputs:\n      artifact-id: ${{ steps.candidate.outputs.artifact-id }}\n",
                      self.producer)
        self.assertNotIn("candidate-locator", self.producer)

    def test_no_gate_depends_on_the_candidate_jobs(self):
        for name in (*EXISTING_JOBS, "development-candidate"):
            with self.subTest(job=name):
                header = job_text(self.source, name).split("    steps:", 1)[0]
                self.assertNotIn("needs: development-candidate", header)

    def test_installs_follow_every_distribution_build(self):
        self.assertIn("    needs: development-candidate\n", self.job)
        condition = " ".join(self.job.split("    runs-on:", 1)[0].split("    if:", 1)[1].split())
        for requirement in ("github.event_name == 'workflow_dispatch'", "inputs.build_candidate",
                            "github.ref_type == 'branch'", "github.event_name == 'pull_request'",
                            "github.event_name == 'push'"):
            self.assertIn(requirement, condition)
        self.assertNotIn("always()", condition)
        self.assertIn("github.event.pull_request.head.repo.full_name == github.repository", condition)

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
        self.assertRegex(self.job, r"    permissions:\n      contents: read\n"
                                  r"      actions: read(?: #[^\n]*)?\n    strategy:")
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
        for setting in (f"ref: {SOURCE_COMMIT}", "fetch-depth: 0", "persist-credentials: false"):
            self.assertIn(setting, checkout)
        for action in SETUP_ACTIONS:
            self.assertIn(f"uses: {action}\n", self.job)
        self.assertIn("python-version: '3.12'", self.job)
        self.assertIn("node-version: '22.x'", self.job)
        for tool in ("setup-go", "make tools"):
            self.assertNotIn(tool, self.job)

    def test_installs_select_the_exact_producer_artifact_id_without_rebuilding(self):
        proof = self.steps["Prove the candidate install through the channel"]
        self.assertIn("ARTIFACT_ID: ${{ needs.development-candidate.outputs.artifact-id }}", proof)
        for argument in ('--artifact-id "${ARTIFACT_ID}"', '--channel "${CHANNEL}"',
                         '--output-dir "${RUNNER_TEMP}/candidate-install-proof"', "--github-auth environment"):
            self.assertIn(argument, proof)
        self.assertNotIn("candidate-locator", self.job)
        self.assertNotIn("build_development_candidate", self.job)

    def test_native_platform_is_checked_before_any_install_work(self):
        names = list(self.steps)
        check = self.steps["Require the intended native platform"]
        self.assertIn("EXPECTED_OS: ${{ matrix.host_os }}", check)
        self.assertIn("EXPECTED_ARCH: ${{ matrix.host_arch }}", check)
        self.assertIn("import platform_identity", check)
        self.assertLess(names.index("Require the intended native platform"),
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

    def test_only_the_actual_install_result_is_uploaded_under_a_unique_name(self):
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
                          for name in ("receipt.json",)])
        for setting in ("overwrite: false", "retention-days: 90"):
            self.assertIn(setting, upload)


if __name__ == "__main__":
    unittest.main()
