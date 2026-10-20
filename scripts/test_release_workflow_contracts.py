"""Cross-tag publication and proof-retention constraints for release workflows."""

from pathlib import Path
import os
import re
import sys
import tempfile
import textwrap
import unittest

from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.test_published_install import PublishedBase, REVISION, TAG


WORKFLOWS = Path(__file__).resolve().parent.parent / ".github/workflows"
NATIVE_GATE = "Validate exact proof and public installation prerequisites"


def workflow_step(source: str, name: str) -> str:
    return source.split(f"      - name: {name}\n", 1)[1].split("\n      - name: ", 1)[0]


def workflow_job(source: str, name: str) -> str:
    jobs = source.split("\njobs:\n", 1)[1]
    return re.split(r"\n  [a-z-]+:\n", jobs.split(f"\n  {name}:\n", 1)[1], maxsplit=1)[0]


# Each publication job and the one job it must wait for; a skipped or failed
# parent therefore stops every later consumer and writer.
PUBLICATION_NEEDS = {"promote": None, "published-npm": "promote",
                     "publish-homebrew": "published-npm", "published-homebrew": "publish-homebrew"}
VALIDATION_STEPS = (NATIVE_GATE, "Validate already published release", "Validate release prerequisites")
# Commands that write the npm publish credential, npm, the GitHub Release, or the tap.
PUBLICATION_WRITES = ("_authToken", "npm publish", "gh release edit", "publish-homebrew-formula.sh")
STATUS_OVERRIDE = re.compile(r"\b(?:always|failure|cancelled)\(\)|!\s*success\(\)")


def job_steps(body: str) -> dict[str, str]:
    return {chunk.split("\n", 1)[0]: chunk for chunk in body.split("\n      - name: ")[1:]}


def publication_gate_violations(source: str) -> list[str]:
    """Name every way a failed validation or parent job could still publish.

    Only job conditions, validation steps, and publication-writing steps are
    gated; a diagnostic or receipt step may still run after a failure.
    """
    violations = []
    for job, parent in PUBLICATION_NEEDS.items():
        body = workflow_job(source, job)
        header = body.split("\n    steps:\n", 1)[0]
        if re.search(r"(?m)^    continue-on-error:", header) or any(
                STATUS_OVERRIDE.search(condition) for condition in re.findall(r"(?m)^    if: (.+)$", header)):
            violations.append(f"{job}: job status override")
        if parent is not None and re.findall(r"(?m)^    needs: (.+)$", header) != [parent]:
            violations.append(f"{job}: needs is not exactly {parent}")
        steps = job_steps(body)
        names = list(steps)
        writers = [name for name, step in steps.items() if any(write in step for write in PUBLICATION_WRITES)]
        validations = [name for name in VALIDATION_STEPS if name in steps]
        if writers and validations and (min(map(names.index, writers))
                                        < max(map(names.index, validations))):
            violations.append(f"{job}: publication write precedes validation")
        for name in (*validations, *writers):
            step = steps[name]
            if re.search(r"(?m)^        continue-on-error:", step) or any(
                    STATUS_OVERRIDE.search(condition) for condition in re.findall(r"(?m)^        if: (.+)$", step)):
                violations.append(f"{job}: {name}: step status override")
    return violations


class ReleaseWorkflowContractTests(unittest.TestCase):
    def test_published_recovery_skips_publication_and_keeps_consumer_dependencies(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        self.assertIn("      resume_published:\n", source)
        for name in (NATIVE_GATE, "Configure npm credentials", "Publish or verify npm package",
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

    def test_promotion_gate_runs_only_the_strict_native_validator(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        self.assertNotIn("accepted_native_failure", source.lower())
        code = textwrap.dedent(workflow_step(source, NATIVE_GATE).split("run: |\n", 1)[1])
        with tempfile.TemporaryDirectory(prefix="release-native-gate-") as directory:
            root = Path(directory)
            fake_bin, runner, record = root / "bin", root / "runner", root / "python-argv"
            fake_bin.mkdir()
            runner.mkdir()
            python = fake_bin / "python3"
            python.write_text(f'#!/bin/sh\nprintf "%s\\n" "$@" > "{record}"\n', encoding="utf-8")
            python.chmod(0o755)
            strict = [".local/release-harness/scripts/validate_native_release.py", "promotion",
                      "--tag", TAG, "--repo", "owner/repository", "--source-revision", REVISION,
                      "--native-validation-run-id", "39403240260",
                      "--work-dir", f"{runner}/native-promotion"]
            for run_id, expected in (("39403240260", 0), ("", 1), ("0", 1), ("012", 1), ("1a", 1)):
                with self.subTest(run_id=run_id):
                    record.unlink(missing_ok=True)
                    env = {"PATH": str(fake_bin) + os.pathsep + os.defpath, "TAG": TAG,
                           "GITHUB_REPOSITORY": "owner/repository", "GITHUB_SHA": REVISION,
                           "NATIVE_VALIDATION_RUN_ID": run_id, "RUNNER_TEMP": str(runner)}
                    result = run_bounded_command(["/bin/bash", "-euo", "pipefail", "-c", code], env,
                                                 max_output=4096, capture=True, timeout=10, cwd=root)
                    self.assertEqual(result.exit_code, expected, result.output.decode())
                    if expected:
                        self.assertFalse(record.exists())
                    else:
                        self.assertEqual(record.read_text(encoding="utf-8").splitlines(), strict)

    def test_validation_precedes_every_publication_write_and_failure_stops_it(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        self.assertEqual(publication_gate_violations(source), [])
        promote, homebrew = workflow_job(source, "promote"), workflow_job(source, "publish-homebrew")
        self.assertEqual(set(VALIDATION_STEPS) - set(job_steps(promote)), set())
        for write in PUBLICATION_WRITES:
            with self.subTest(write=write):
                self.assertIn(write, promote + homebrew)

    def test_publication_gate_permits_diagnostics_but_refuses_every_bypass(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        writer, gate = "      - name: Publish GitHub Release\n", f"      - name: {NATIVE_GATE}\n"
        diagnostic = ("      - name: Retain promotion diagnostics\n        if: always()\n"
                      "        continue-on-error: true\n        uses: actions/upload-artifact@v4\n")
        self.assertEqual(publication_gate_violations(source.replace(writer, diagnostic + writer)), [])
        refused = {
            "promote: Validate exact proof and public installation prerequisites: step status override":
                source.replace(gate, gate + "        continue-on-error: true\n"),
            "promote: Publish GitHub Release: step status override":
                source.replace(writer + "        if: ${{ !inputs.resume_published }}",
                               writer + "        if: ${{ !cancelled() && !inputs.resume_published }}"),
            "publish-homebrew: job status override":
                source.replace("    needs: published-npm\n", "    needs: published-npm\n    if: always()\n"),
            "publish-homebrew: needs is not exactly published-npm":
                source.replace("    needs: published-npm\n", "    needs: promote\n"),
            "promote: publication write precedes validation":
                source.replace(gate, "      - name: Early release edit\n        run: gh release edit x\n" + gate),
        }
        for expected, mutated in refused.items():
            with self.subTest(expected=expected):
                self.assertNotEqual(mutated, source)
                self.assertIn(expected, publication_gate_violations(mutated))

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
