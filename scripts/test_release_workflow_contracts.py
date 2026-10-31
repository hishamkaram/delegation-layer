"""Single trusted-job publication, publication lock and proof-retention constraints for release workflows,
and the one standard Python discovery that ``make check`` runs."""

import contextlib
import io
from pathlib import Path
import os
import re
import shlex
import textwrap
import unittest
from unittest import mock

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError
from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.test_candidate_source_support import ChildFault, settle_retained


WORKFLOWS = Path(__file__).resolve().parent.parent / ".github/workflows"


def workflow_step(source: str, name: str) -> str:
    return source.split(f"      - name: {name}\n", 1)[1].split("\n      - name: ", 1)[0]


def workflow_job(source: str, name: str) -> str:
    jobs = "\n" + source.split("\njobs:\n", 1)[1]
    return re.split(r"\n  [a-z-]+:\n", jobs.split(f"\n  {name}:\n", 1)[1], maxsplit=1)[0]


# The one trusted-main publication job and its one effect step.
PUBLICATION_JOB = "publish"
PUBLISH_STEP = "Publish the tested distribution bytes"
RECEIPT_STEP = "Retain publication result"
PUBLISH_COMMAND = ('python3 scripts/publish_development_candidate.py '
                   '--output-dir "${RUNNER_TEMP}/distribution-publication"')
# Retired tag-push, rebuild, repack, attestation and post-publication install or behavior work.
RETIRED = ("push:", "tags:", "setup-go", "make ", "goreleaser", "npm pack", "npm publish", "npm install",
           "gh release", "attest", "id-token", "release-consumer.yml", "publish-homebrew-formula.sh",
           "validate_native_release.py", "fetch-pueue-assets.sh", "archive-smoke")
STATUS_OVERRIDE = re.compile(r"\b(?:always|failure|cancelled)\(\)|!\s*success\(\)")


def conditions(text: str, indent: int) -> list[str]:
    """Return each `if:` at one key indent, with its more-indented block-scalar lines.

    Whitespace-only lines stay inside the block while a more-indented line follows.
    """
    return re.findall(rf"(?m)^ {{{indent}}}if:(.*(?:\n(?:[ \t]*\n)*[ ]{{{indent + 1},}}\S.*)*)", text)


def job_steps(body: str) -> dict[str, str]:
    return {chunk.split("\n", 1)[0]: chunk for chunk in body.split("\n      - name: ")[1:]}


def step_script(step: str) -> str:
    inline = re.search(r"(?m)^        run: (?!\|)(.+)$", step)
    if inline:
        return inline.group(1).strip()
    return textwrap.dedent(step.split("run: |\n", 1)[1])


def publication_gate_violations(source: str) -> list[str]:
    """Name every way the one publication effect could run late, conditionally or beside other work.

    Only the receipt upload may run after a failure; it writes no destination.
    """
    violations = []
    jobs = re.findall(r"(?m)^  ([a-z][a-z0-9-]*):\n", "\n" + source.split("\njobs:\n", 1)[1])
    if jobs != [PUBLICATION_JOB]:
        violations.append(f"jobs are not exactly {PUBLICATION_JOB}")
        return violations
    body = workflow_job(source, PUBLICATION_JOB)
    header = body.split("\n    steps:\n", 1)[0]
    if re.search(r"(?m)^    continue-on-error:", header) or any(
            STATUS_OVERRIDE.search(condition) for condition in conditions(header, 4)):
        violations.append("publish: job status override")
    steps = job_steps(body)
    if PUBLISH_STEP not in steps:
        return [*violations, "publish: publication step missing"]
    step = steps[PUBLISH_STEP]
    if step_script(step) != PUBLISH_COMMAND:
        violations.append("publish: publication step is not the trusted CLI")
    if conditions(step, 8) or re.search(r"(?m)^        continue-on-error:", step):
        violations.append("publish: publication step is conditional or tolerates failure")
    names = list(steps)
    if names[names.index(PUBLISH_STEP) + 1:] != [RECEIPT_STEP]:
        violations.append("publish: work follows the publication step")
    for name in names[:names.index(PUBLISH_STEP)]:
        if "run:" in steps[name] or not any(setup in steps[name] for setup in (
                "actions/checkout@", "actions/setup-python@", "actions/setup-node@")):
            violations.append(f"publish: {name} precedes publication but is not setup")
    retired = [token for token in RETIRED if token in source]
    if retired:
        violations.append("retired release work present: " + ",".join(retired))
    return violations


def run_publish_step(tree: FixtureTree, environment: dict[str, str]) -> tuple[int, list[str], bool]:
    """Run the extracted publication step over a recording `python3`; return exit, argv and output-dir state.

    Every file lives under the caller's tree, which observes the step's original child failure.
    """
    source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
    script = step_script(workflow_step(source, PUBLISH_STEP))
    root = tree.root
    fake_bin, runner, record = root / "bin", root / "runner", root / "python-argv"
    fake_bin.mkdir()
    runner.mkdir()
    python = fake_bin / "python3"
    python.write_text(f'#!/bin/sh\nprintf "%s\\n" "$@" > "{record}"\n', encoding="utf-8")
    python.chmod(0o755)
    env = {"PATH": str(fake_bin) + os.pathsep + os.defpath, "RUNNER_TEMP": str(runner), **environment}
    result = tree.observe_operation(lambda: run_bounded_command(
        ["/bin/bash", "-euo", "pipefail", "-c", script], env, max_output=4096, capture=True, timeout=10,
        cwd=root))
    argv = record.read_text(encoding="utf-8").splitlines() if record.exists() else []
    return result.exit_code, [item.replace(str(runner), "${RUNNER_TEMP}") for item in argv], (
        runner / "distribution-publication").exists()


class ReleaseWorkflowContractTests(unittest.TestCase):
    def publish_tree(self) -> FixtureTree:
        tree = FixtureTree.open(prefix="release-publish-step", label="release publish step tree")
        self.addCleanup(tree.dispose)
        return tree

    def test_one_trusted_job_publishes_once_with_no_retired_release_work(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        self.assertEqual(publication_gate_violations(source), [])
        self.assertEqual(re.findall(r"(?m)^  ([a-z_]+):$", source.split("\non:\n", 1)[1].split("\n    inputs:", 1)[0]),
                         ["workflow_dispatch"])
        self.assertIn("    if: ${{ github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' }}\n",
                      workflow_job(source, PUBLICATION_JOB))

    def test_publication_gate_refuses_every_bypass_and_retired_work(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        header = f"      - name: {PUBLISH_STEP}\n"
        receipt = f"      - name: {RECEIPT_STEP}\n"
        refused = {
            "publish: publication step is conditional or tolerates failure": [
                source.replace(header, header + "        continue-on-error: true\n"),
                source.replace(header, header + "        if: ${{ always() }}\n"),
                source.replace(header, header + "        if: >-\n          inputs.resume_published != 'x'\n")],
            "publish: publication step is not the trusted CLI": [
                source.replace(PUBLISH_COMMAND, PUBLISH_COMMAND + " || true"),
                source.replace(PUBLISH_COMMAND, "python3 .local/candidate/scripts/publish_development_candidate.py")],
            "publish: job status override": [
                source.replace("    timeout-minutes: 60\n", "    timeout-minutes: 60\n    continue-on-error: true\n"),
                source.replace("    timeout-minutes: 60\n", "    timeout-minutes: 60\n    if: >-\n      always()\n")],
            "publish: work follows the publication step": [
                source.replace(receipt, "      - name: Verify the install\n        run: npx delegation-layer\n\n"
                                        + receipt)],
            "publish: Build precedes publication but is not setup": [
                source.replace(header, "      - name: Build\n        run: make build\n\n" + header)],
            "jobs are not exactly publish": [
                source + "\n  published-npm:\n    needs: publish\n    runs-on: ubuntu-24.04\n"],
            "retired release work present: goreleaser": [source.replace(PUBLISH_COMMAND, PUBLISH_COMMAND + " # goreleaser")],
        }
        for expected, variants in refused.items():
            for index, mutated in enumerate(variants):
                with self.subTest(expected=expected, variant=index):
                    self.assertNotEqual(mutated, source)
                    self.assertIn(expected, publication_gate_violations(mutated))

    def test_the_publication_step_runs_only_the_trusted_cli_with_its_owned_output(self):
        exit_code, argv, created = run_publish_step(self.publish_tree(), {})
        self.assertEqual(exit_code, 0)
        self.assertEqual(argv, ["scripts/publish_development_candidate.py", "--output-dir",
                                "${RUNNER_TEMP}/distribution-publication"])
        self.assertFalse(created)

    def test_an_unproven_publication_step_child_retains_its_tree(self):
        tree = self.publish_tree()
        fault = ChildFault(lambda argv: argv[:1] == ["/bin/bash"])
        with mock.patch(f"{__name__}.run_bounded_command", fault), self.assertRaises(BoundedCommandError) as caught:
            run_publish_step(tree, {})
        self.assertIs(tree.unresolved, caught.exception)
        with contextlib.redirect_stderr(io.StringIO()):
            self.doCleanups()
        self.assertEqual(fault.launched, 0)
        settle_retained(self, tree, fault, (tree.root / "bin" / "python3", tree.root / "runner"))

    def test_every_dispatch_uses_one_workflow_wide_publication_lock(self):
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

REPOSITORY = WORKFLOWS.parent.parent
# The one discovery of every Python test; standard unittest owns which files it selects and runs.
PYTHON_DISCOVERY = "python3 -m unittest discover -s scripts -t scripts -p 'test_*.py' -v"
VALIDATORS = ("python3 scripts/verify_skills.py", "python3 scripts/verify_acceptance_ownership.py")
UNITTEST = re.compile(r"-m\s+unittest\b")
SELECTED_STUB = "import unittest\n\n\nclass Selected(unittest.TestCase):\n    def test_selected(self):\n" \
                "        print(__file__)\n"


def make_dry_run(tree: FixtureTree, makefile: str, target: str) -> list[str]:
    """The commands Make itself expands for ``target`` over ``makefile``; a dry run starts only sub-makes."""
    (tree.root / "Makefile").write_text(makefile, encoding="utf-8")
    env = {"PATH": os.environ["PATH"], "HOME": str(tree.root), "LC_ALL": "C"}
    result = tree.observe_operation(lambda: run_bounded_command(
        ["make", "--no-print-directory", "-n", target], env, max_output=1 << 16, capture=True, timeout=60,
        cwd=tree.root))
    if result.exit_code:
        raise AssertionError(f"make -n {target} exited {result.exit_code}")
    return result.output.decode("utf-8").splitlines()


def replace_once(text: str, old: str, new: str) -> str:
    """One mutation at one anchor; an absent or repeated anchor is a broken control, not a refusal."""
    if text.count(old) != 1:
        raise AssertionError(f"mutation anchor occurs {text.count(old)} times: {old!r}")
    return text.replace(old, new)


def unittest_commands(lines: list[str]) -> list[str]:
    return [line for line in lines if UNITTEST.search(line)]


def selected_test_files(tree: FixtureTree, layout: Path, extra: tuple[str, ...] = ()) -> list[str]:
    """Run the standard discovery over a mirror of ``layout``; return each selected file relative to it.

    Every package marker becomes an empty marker and every ``test_*.py`` file, plus each
    ``extra`` path, becomes a one-test stub, so the real loader alone decides what runs.
    The mirror is refused once a real package customizes selection through ``load_tests``.
    """
    mirror = tree.root / "scripts"
    for marker in layout.rglob("__init__.py"):
        if "load_tests" in marker.read_text(encoding="utf-8"):
            raise AssertionError(f"{marker} customizes discovery; the mirror cannot reproduce it")
        (mirror / marker.relative_to(layout)).parent.mkdir(parents=True, exist_ok=True)
        (mirror / marker.relative_to(layout)).touch()
    for relative in [*(path.relative_to(layout) for path in layout.rglob("test_*.py")), *map(Path, extra)]:
        (mirror / relative).parent.mkdir(parents=True, exist_ok=True)
        (mirror / relative).write_text(SELECTED_STUB, encoding="utf-8")
    env = {"PATH": os.environ["PATH"], "HOME": str(tree.root), "LC_ALL": "C", "PYTHONDONTWRITEBYTECODE": "1"}
    result = tree.observe_operation(lambda: run_bounded_command(
        shlex.split(PYTHON_DISCOVERY), env, max_output=1 << 20, capture=True, timeout=60, cwd=tree.root))
    if result.exit_code:
        raise AssertionError(f"standard discovery over the mirror exited {result.exit_code}")
    return [Path(line).relative_to(mirror).as_posix() for line in result.output.decode("utf-8").splitlines()]


class PythonGateContractTests(unittest.TestCase):
    """``make check`` runs every Python test exactly once, through the one standard discovery."""

    def tree(self) -> FixtureTree:
        tree = FixtureTree.open(prefix="python-gate-contract", label="python gate contract tree")
        self.addCleanup(tree.dispose)
        return tree

    def test_check_runs_one_standard_discovery_and_the_validators_without_scoped_tests(self):
        makefile = (REPOSITORY / "Makefile").read_text(encoding="utf-8")
        check = make_dry_run(self.tree(), makefile, "check")
        self.assertEqual(unittest_commands(check), [PYTHON_DISCOVERY])
        self.assertEqual([line for line in check if line in VALIDATORS], list(VALIDATORS))
        self.assertEqual(make_dry_run(self.tree(), makefile, "test-python"), [PYTHON_DISCOVERY])

    def test_check_wiring_refuses_an_omitted_duplicated_scoped_or_narrowed_discovery(self):
        makefile = (REPOSITORY / "Makefile").read_text(encoding="utf-8")
        gate = "\t$(MAKE) test-python\n"
        validator = "\t$(VERIFY_SKILLS)\n\t$(MAKE) verify-diagram-checksums\n"
        refused = {
            "omitted": (replace_once(makefile, gate, ""), []),
            "duplicated": (replace_once(makefile, gate, gate * 2), [PYTHON_DISCOVERY] * 2),
            "scoped beside it": (replace_once(makefile, gate, gate + "\t$(MAKE) test-evidence-tools\n"), None),
            "validator bundle": (replace_once(makefile, validator, validator.replace(
                "$(VERIFY_SKILLS)", "$(MAKE) verify-skills")), None),
            "narrowed": (replace_once(makefile, PYTHON_DISCOVERY, PYTHON_DISCOVERY.replace(
                "test_*", "test_candidate_*")), None),
        }
        for name, (mutated, expected) in refused.items():
            with self.subTest(mutation=name):
                commands = unittest_commands(make_dry_run(self.tree(), mutated, "check"))
                self.assertNotEqual(commands, [PYTHON_DISCOVERY])
                if expected is not None:
                    self.assertEqual(commands, expected)

    def test_standard_discovery_selects_every_test_file_once_including_new_modules(self):
        layout = REPOSITORY / "scripts"
        future = ("test_future_gate_module.py", "acceptance_tests/test_future_gate_module.py",
                  "acceptance_tests/native/test_future_gate_module.py")
        hidden = "unpackaged_fixtures/test_hidden_gate_module.py"
        selected = selected_test_files(self.tree(), layout, (*future, hidden))
        on_disk = sorted(path.relative_to(layout).as_posix() for path in layout.rglob("test_*.py"))
        self.assertEqual(len(selected), len(set(selected)), "a test file was selected more than once")
        self.assertEqual(sorted(set(selected) - {hidden}), sorted([*on_disk, *future]))
        self.assertNotIn(hidden, selected, "a test file outside any package is never discovered")


if __name__ == "__main__":
    unittest.main()
