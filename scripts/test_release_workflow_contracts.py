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
PUBLIC_PRECHECK = "Require a public repository for anonymous installation"


def workflow_step(source: str, name: str) -> str:
    return source.split(f"      - name: {name}\n", 1)[1].split("\n      - name: ", 1)[0]


def workflow_job(source: str, name: str) -> str:
    jobs = "\n" + source.split("\njobs:\n", 1)[1]
    return re.split(r"\n  [a-z-]+:\n", jobs.split(f"\n  {name}:\n", 1)[1], maxsplit=1)[0]


# Each publication job and the one job it must wait for; a skipped or failed
# parent therefore stops every later consumer and writer.
PUBLICATION_NEEDS = {"promote": None, "published-npm": "promote",
                     "publish-homebrew": "published-npm", "published-homebrew": "publish-homebrew"}
VALIDATION_STEPS = (NATIVE_GATE, "Validate already published release", "Validate release prerequisites")
# Commands that write the npm publish credential, npm, the GitHub Release, or the tap.
PUBLICATION_WRITES = ("_authToken", "npm publish", "gh release edit", "publish-homebrew-formula.sh")
STATUS_OVERRIDE = re.compile(r"\b(?:always|failure|cancelled)\(\)|!\s*success\(\)")


def conditions(text: str, indent: int) -> list[str]:
    """Return each `if:` at one key indent, with its more-indented block-scalar lines.

    Whitespace-only lines stay inside the block while a more-indented line follows.
    """
    return re.findall(rf"(?m)^ {{{indent}}}if:(.*(?:\n(?:[ \t]*\n)*[ ]{{{indent + 1},}}\S.*)*)", text)


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
                STATUS_OVERRIDE.search(condition) for condition in conditions(header, 4)):
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
                    STATUS_OVERRIDE.search(condition) for condition in conditions(step, 8)):
                violations.append(f"{job}: {name}: step status override")
    return violations


RECOVERY = "Validate already published release"
PRODUCER_COMMAND = 'python3 scripts/validate_native_release.py public-prerequisite --repo "${GITHUB_REPOSITORY}"'
# Recovery runs the trusted main harness, never code from the release tag being recovered.
RECOVERY_COMMAND = ('python3 .local/release-harness/scripts/validate_native_release.py '
                    'public-prerequisite --repo "${GITHUB_REPOSITORY}"')
PUBLIC_FACTS = '{"private": false, "visibility": "public"}'
POLICY_CODE = "anonymous_installation_requires_public_repository"
REFUSED_FACTS = ('{"private": true, "visibility": "private"}',
                 '{"private": false, "visibility": "internal"}',
                 '{"private": false, "visibility": "private"}',
                 '{"private": true, "visibility": "public"}',
                 '{"private": false}', '{"visibility": "public"}', '{}',
                 '{"private": "false", "visibility": "public"}',
                 '{"private": null, "visibility": "public"}')
MALFORMED_FACTS = ("[]", "null", "not json")
REPOSITORY_ENDPOINT = "repos/owner/repository"
RECOVERY_ENDPOINTS = [REPOSITORY_ENDPOINT, f"{REPOSITORY_ENDPOINT}/releases/tags/{TAG}",
                      f"{REPOSITORY_ENDPOINT}/releases/latest"]
FAKE_GH = r"""#!/bin/sh
printf '%s\n' "$*" >> "$GH_RECORD"
[ "$GH_TOKEN" = ci-token ] || exit 3
case "$*" in
  *"/releases/latest"*) printf '%s\n' "$LATEST_RELEASE" ;;
  *"/releases/tags/"*) printf '%s\n' "$PUBLISHED_READY" ;;
  *) [ "$GH_EXIT" = 0 ] || exit "$GH_EXIT"; printf '%s' "$REPOSITORY_FACTS" ;;
esac
"""
POISONED_TAG_SCRIPT = "import sys\nprint('POISONED TAG SCRIPT RAN')\nsys.exit(0)\n"


def step_script(step: str) -> str:
    inline = re.search(r"(?m)^        run: (?!\|)(.+)$", step)
    if inline:
        return inline.group(1).strip()
    return textwrap.dedent(step.split("run: |\n", 1)[1])


def run_release_step(name: str, *, facts: str = PUBLIC_FACTS, gh_exit: int = 0,
                     token: str | None = "ci-token", latest: str = TAG, ready: str = "true",
                     tag_side: str | None = None) -> tuple[int, str, list[str]]:
    """Run one extracted workflow step over a fake `gh`; return exit code, output, gh endpoints.

    The real CLI, transport and policy run. Recovery runs in a tag-checkout stand-in whose
    `.local/release-harness` is this source tree; `tag_side` plants a poisoned tag script.
    """
    source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
    script = "exec 2>&1\n" + step_script(workflow_step(source, name))
    repo_root = WORKFLOWS.parent.parent
    with tempfile.TemporaryDirectory(prefix="release-step-") as directory:
        root = Path(directory)
        fake_bin, record, checkout = root / "bin", root / "gh-calls", root / "tag-checkout"
        fake_bin.mkdir()
        checkout.mkdir()
        (fake_bin / "python3").symlink_to(sys.executable)
        gh = fake_bin / "gh"
        gh.write_text(FAKE_GH, encoding="utf-8")
        gh.chmod(0o755)
        if name == RECOVERY:
            (checkout / ".local").mkdir()
            (checkout / ".local" / "release-harness").symlink_to(repo_root, target_is_directory=True)
            if tag_side == "poisoned":
                (checkout / "scripts").mkdir()
                (checkout / "scripts" / "validate_native_release.py").write_text(
                    POISONED_TAG_SCRIPT, encoding="utf-8")
        env = {"PATH": str(fake_bin) + os.pathsep + os.defpath, "GH_RECORD": str(record),
               "GH_EXIT": str(gh_exit), "REPOSITORY_FACTS": facts, "TAG": TAG,
               "LATEST_RELEASE": latest, "PUBLISHED_READY": ready,
               "GITHUB_REPOSITORY": "owner/repository", "PYTHONDONTWRITEBYTECODE": "1"}
        if token is not None:
            env["GH_TOKEN"] = token
        result = run_bounded_command(["/bin/bash", "-e", "-c", script], env, max_output=4096,
                                     capture=True, timeout=30,
                                     cwd=checkout if name == RECOVERY else repo_root)
        calls = record.read_text(encoding="utf-8").splitlines() if record.exists() else []
    output = result.output.decode()
    endpoints = [next(word for word in call.split() if word.startswith("repos/")) for call in calls]
    return result.exit_code, output, endpoints


def public_precheck_violations(source: str) -> list[str]:
    """Name every way the tag release could create a draft before the visibility check.

    Only checkout and Python setup may run before the check; neither the check
    nor its job may be skipped, conditioned, or allowed to fail.
    """
    body = workflow_job(source, "release")
    steps = job_steps(body)
    if PUBLIC_PRECHECK not in steps:
        return ["release: public repository precheck missing"]
    names = list(steps)
    violations = []
    for name in names[:names.index(PUBLIC_PRECHECK)]:
        if not any(setup in steps[name] for setup in ("actions/checkout@", "actions/setup-python@")):
            violations.append(f"release: {name} precedes the public repository precheck")
    step, header = steps[PUBLIC_PRECHECK], body.split("\n    steps:\n", 1)[0]
    if step_script(step) != PRODUCER_COMMAND:
        violations.append("release: public repository precheck is not the canonical command")
    if conditions(step, 8) or re.search(r"(?m)^        continue-on-error:", step):
        violations.append("release: public repository precheck is conditional or tolerates failure")
    if re.search(r"(?m)^    continue-on-error:", header) or any(
            STATUS_OVERRIDE.search(condition) for condition in conditions(header, 4)):
        violations.append("release: job status override")
    return violations


def recovery_public_violations(source: str) -> list[str]:
    """Name every way recovery could skip, weaken, or reorder the public-repository check.

    The step stays conditional on `inputs.resume_published`; that is its one allowed condition.
    """
    steps = job_steps(workflow_job(source, "promote"))
    if RECOVERY not in steps:
        return ["promote: recovery validation step missing"]
    step, violations = steps[RECOVERY], []
    script = step_script(step)
    if script.splitlines()[0].strip() != RECOVERY_COMMAND:
        violations.append("promote: recovery does not start with the trusted public prerequisite")
    if ".private" in script:
        violations.append("promote: recovery repeats the legacy private-flag check")
    if [condition.strip() for condition in conditions(step, 8)] != ["${{ inputs.resume_published }}"]:
        violations.append("promote: recovery condition is not exactly resume_published")
    if re.search(r"(?m)^        continue-on-error:", step):
        violations.append("promote: recovery tolerates failure")
    names = list(steps)
    if names.index(RECOVERY) > names.index("Validate release prerequisites"):
        violations.append("promote: recovery follows the release prerequisites")
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
        folded_diagnostic = diagnostic.replace("if: always()", "if: >-\n          always()")
        blank_diagnostic = diagnostic.replace("if: always()", "if: >-\n\n          always()")
        for permitted in (diagnostic, folded_diagnostic, blank_diagnostic):
            with self.subTest(permitted=permitted):
                self.assertEqual(publication_gate_violations(source.replace(writer, permitted + writer)), [])
        writer_condition = writer + "        if: ${{ !inputs.resume_published }}"
        homebrew_needs = "    needs: published-npm\n"
        refused = {
            "promote: Validate exact proof and public installation prerequisites: step status override": [
                source.replace(gate, gate + "        continue-on-error: true\n")],
            "promote: Publish GitHub Release: step status override": [
                source.replace(writer_condition,
                               writer + "        if: ${{ !cancelled() && !inputs.resume_published }}"),
                source.replace(writer_condition, writer + "        if: >-\n          always()"),
                source.replace(writer_condition, writer + "        if: |\n          ${{ always() }}"),
                source.replace(writer_condition, writer + "        if: >-\n\n          always()"),
                source.replace(writer_condition,
                               writer + "        if: >-\n          ${{ !inputs.resume_published ||\n  \n"
                                        "          always() }}")],
            "publish-homebrew: job status override": [
                source.replace(homebrew_needs, homebrew_needs + "    if: always()\n"),
                source.replace(homebrew_needs, homebrew_needs + "    if: >-\n      always()\n"),
                source.replace(homebrew_needs, homebrew_needs + "    if: |\n      !success()\n"),
                source.replace(homebrew_needs, homebrew_needs + "    if: |\n\n      always()\n"),
                source.replace(homebrew_needs, homebrew_needs + "    if: >-\n      github.ref != ''\n\n"
                                                                "      || always()\n")],
            "publish-homebrew: needs is not exactly published-npm": [
                source.replace(homebrew_needs, "    needs: promote\n")],
            "promote: publication write precedes validation": [
                source.replace(gate, "      - name: Early release edit\n        run: gh release edit x\n" + gate)],
        }
        for expected, variants in refused.items():
            for index, mutated in enumerate(variants):
                with self.subTest(expected=expected, variant=index):
                    self.assertNotEqual(mutated, source)
                    self.assertIn(expected, publication_gate_violations(mutated))

    def test_public_precheck_precedes_every_release_effect_and_cannot_be_bypassed(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        self.assertEqual(public_precheck_violations(source), [])
        step = workflow_step(source, PUBLIC_PRECHECK)
        self.assertIn("GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}", step)
        self.assertEqual(step_script(step), PRODUCER_COMMAND)
        # The canonical CLI owns the visibility decision; no workflow shell or inline code repeats it.
        self.assertNotIn(".private", source)
        self.assertNotIn("visibility", source)
        header = f"      - name: {PUBLIC_PRECHECK}\n"
        block = f"{header}{step}\n"
        self.assertIn(block, source)
        without = source.replace(block, "")
        node, tools = "      - name: Set up Node.js\n", "      - name: Run repository checks\n"
        refused = {
            "release: public repository precheck missing": [without],
            "release: Set up Go precedes the public repository precheck": [
                without.replace(node, block + node)],
            "release: Install pinned tools precedes the public repository precheck": [
                without.replace(tools, block + tools)],
            "release: public repository precheck is not the canonical command": [
                source.replace(PRODUCER_COMMAND, "echo skipped"),
                source.replace(PRODUCER_COMMAND, PRODUCER_COMMAND + " || true"),
                source.replace(PRODUCER_COMMAND, RECOVERY_COMMAND)],
            "release: public repository precheck is conditional or tolerates failure": [
                source.replace(header, header + "        continue-on-error: true\n"),
                source.replace(header, header + "        if: ${{ always() }}\n"),
                source.replace(header, header + "        if: ${{ github.ref_name != 'v0.0.0' }}\n")],
            "release: job status override": [
                source.replace("    timeout-minutes: 45\n",
                               "    timeout-minutes: 45\n    continue-on-error: true\n", 1)],
        }
        for expected, variants in refused.items():
            for index, mutated in enumerate(variants):
                with self.subTest(expected=expected, variant=index):
                    self.assertNotEqual(mutated, source)
                    self.assertIn(expected, public_precheck_violations(mutated))

    def test_recovery_runs_the_trusted_public_prerequisite_first_and_cannot_be_bypassed(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        self.assertEqual(recovery_public_violations(source), [])
        step = workflow_step(source, RECOVERY)
        for retained in ("releases/tags/", ".draft == false and .prerelease == false",
                         "releases/latest", "Consumer recovery requires an already published stable release.",
                         "Consumer recovery requires the latest stable release to prevent a tap downgrade."):
            self.assertIn(retained, step)
        command_line = f"          {RECOVERY_COMMAND}\n"
        latest_guard = '          if [[ "$(gh api "repos/${GITHUB_REPOSITORY}/releases/latest"'
        condition = "        if: ${{ inputs.resume_published }}\n"
        legacy = ('          if [[ "$(gh api "repos/${GITHUB_REPOSITORY}" --jq .private)" != "false" ]]; then\n'
                  "            exit 1\n          fi\n")
        without = source.replace(command_line, "")
        refused = {
            "promote: recovery does not start with the trusted public prerequisite": [
                without,
                without.replace(latest_guard, command_line + latest_guard),
                source.replace(RECOVERY_COMMAND, PRODUCER_COMMAND),
                source.replace(RECOVERY_COMMAND, RECOVERY_COMMAND + " || true"),
                source.replace(RECOVERY_COMMAND, "true")],
            "promote: recovery repeats the legacy private-flag check": [
                source.replace(command_line, legacy),
                source.replace(command_line, command_line + legacy)],
            "promote: recovery condition is not exactly resume_published": [
                source.replace(condition, ""),
                source.replace(condition, "        if: ${{ !inputs.resume_published }}\n"),
                source.replace(condition, "        if: ${{ inputs.resume_published || always() }}\n")],
            "promote: recovery tolerates failure": [
                source.replace(condition, condition + "        continue-on-error: true\n")],
        }
        for expected, variants in refused.items():
            for index, mutated in enumerate(variants):
                with self.subTest(expected=expected, variant=index):
                    self.assertNotEqual(mutated, source)
                    self.assertIn(expected, recovery_public_violations(mutated))

    def test_both_public_prerequisites_admit_only_public_facts_through_the_actual_cli(self):
        for name, public_endpoints in ((PUBLIC_PRECHECK, [REPOSITORY_ENDPOINT]),
                                       (RECOVERY, RECOVERY_ENDPOINTS)):
            with self.subTest(name=name, facts=PUBLIC_FACTS):
                exit_code, output, endpoints = run_release_step(name)
                self.assertEqual(exit_code, 0, output)
                self.assertEqual(endpoints, public_endpoints)
            for facts in REFUSED_FACTS:
                with self.subTest(name=name, facts=facts):
                    exit_code, output, endpoints = run_release_step(name, facts=facts)
                    self.assertEqual(exit_code, 1, output)
                    self.assertIn(f"Public repository prerequisite failed: {POLICY_CODE} "
                                  "(public-release-precheck)", output)
                    self.assertIn("Make the repository public, then rerun this release workflow.", output)
                    self.assertEqual(endpoints, [REPOSITORY_ENDPOINT])
            for facts in MALFORMED_FACTS:
                with self.subTest(name=name, malformed=facts):
                    exit_code, output, endpoints = run_release_step(name, facts=facts)
                    self.assertEqual(exit_code, 1, output)
                    self.assertNotIn(POLICY_CODE, output)
                    self.assertEqual(endpoints, [REPOSITORY_ENDPOINT])

    def test_both_public_prerequisites_report_api_and_auth_failure_without_guessing(self):
        for name in (PUBLIC_PRECHECK, RECOVERY):
            with self.subTest(name=name, failure="api"):
                exit_code, output, endpoints = run_release_step(name, gh_exit=1)
                self.assertEqual(exit_code, 1, output)
                self.assertIn("github_request_failed", output)
                for guess in (POLICY_CODE, "Make the repository public"):
                    self.assertNotIn(guess, output)
                self.assertEqual(endpoints, [REPOSITORY_ENDPOINT])
            with self.subTest(name=name, failure="no token"):
                exit_code, output, endpoints = run_release_step(name, token=None)
                self.assertEqual(exit_code, 1, output)
                self.assertIn("github_auth_unavailable", output)
                self.assertEqual(endpoints, [])
            with self.subTest(name=name, failure="refusal output"):
                _, output, _ = run_release_step(name, facts=REFUSED_FACTS[0])
                self.assertNotIn("ci-token", output)

    def test_recovery_uses_the_trusted_harness_even_when_the_tag_script_is_poisoned_or_missing(self):
        for tag_side in (None, "poisoned"):
            with self.subTest(tag_side=tag_side, facts="private"):
                exit_code, output, endpoints = run_release_step(
                    RECOVERY, facts=REFUSED_FACTS[0], tag_side=tag_side)
                self.assertEqual(exit_code, 1, output)
                self.assertIn(POLICY_CODE, output)
                self.assertNotIn("POISONED", output)
                self.assertEqual(endpoints, [REPOSITORY_ENDPOINT])
            with self.subTest(tag_side=tag_side, facts="public"):
                exit_code, output, endpoints = run_release_step(RECOVERY, tag_side=tag_side)
                self.assertEqual(exit_code, 0, output)
                self.assertNotIn("POISONED", output)
                self.assertEqual(endpoints, RECOVERY_ENDPOINTS)

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
        for changes, expected, endpoints in (
                ({}, 0, RECOVERY_ENDPOINTS),
                ({"facts": REFUSED_FACTS[0]}, 1, RECOVERY_ENDPOINTS[:1]),
                ({"ready": "false"}, 1, RECOVERY_ENDPOINTS[:2]),
                ({"latest": "v9.9.9"}, 1, RECOVERY_ENDPOINTS)):
            with self.subTest(changes=changes):
                exit_code, output, called = run_release_step(RECOVERY, **changes)
                self.assertEqual(exit_code, expected, output)
                self.assertEqual(called, endpoints)

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
