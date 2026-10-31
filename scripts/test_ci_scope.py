"""Conservative CI selection against real Git ranges, file modes and command failures."""

from pathlib import Path
import os
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from scripts import ci_scope
from scripts.test_candidate_build_workflow import job_text, steps


class ContentPolicyTests(unittest.TestCase):
    def test_only_known_content_families_are_light(self):
        light = (
            "README.md", "CONTRIBUTING.md", "AGENTS.md", "docs/installation.md",
            "agents/_data/product-boundary.md", "skills/docs-updater/SKILL.md",
            "skills/agent-integration/references/interaction.md",
            "plugins/delegation-layer/skills/agent-integration/SKILL.md",
            "plugins/delegation-layer/skills/agent-integration/references/workspaces.md",
            "diagrams/components.architecture.json", "diagrams/lead-interface.dark.png",
            "diagrams/SHA256SUMS",
        )
        for path in light:
            with self.subTest(path=path):
                self.assertFalse(ci_scope.requires_full((ci_scope.Change(path, "100644", "100644", "M"),)))
        full = (
            "UNFAMILIAR.md", "internal/app/README.md", "scripts/README.md",
            "docs/assets/delegation-layer-hero.jpg", "docs/assets/notes.md",
            "docs/something.json", "skills/new-skill/SKILL.md",
            "skills/agent-integration/routing-rule.json", "skills/agent-integration/evals/cases.json",
            "plugins/delegation-layer/hooks/session-start.sh", "diagrams/new.html",
            "Makefile", ".github/workflows/ci.yml", "scripts/ci_scope.py",
            "go.mod", "package.json", "install.sh", "internal/app/app.go",
        )
        for path in full:
            with self.subTest(path=path):
                self.assertTrue(ci_scope.requires_full((ci_scope.Change(path, "100644", "100644", "M"),)))

    def test_empty_mixed_mode_and_type_changes_are_full(self):
        self.assertTrue(ci_scope.requires_full(()))
        ordinary = ci_scope.Change("README.md", "100644", "100644", "M")
        code = ci_scope.Change("cmd/delegate/main.go", "100644", "100644", "M")
        self.assertTrue(ci_scope.requires_full((ordinary, code)))
        for old, new, status in (("100644", "100755", "M"), ("100755", "100755", "M"),
                                 ("100644", "120000", "T"), ("160000", "160000", "M")):
            with self.subTest(old=old, new=new, status=status):
                self.assertTrue(ci_scope.requires_full((ci_scope.Change("docs/guide.md", old, new, status),)))
        self.assertFalse(ci_scope.requires_full((ci_scope.Change("docs/new.md", "000000", "100644", "A"),)))
        self.assertFalse(ci_scope.requires_full((ci_scope.Change("docs/old.md", "100644", "000000", "D"),)))

    def test_unsafe_paths_are_not_content_exemptions(self):
        for path in ("/README.md", "docs/../code.md", "./README.md", "docs//guide.md", ""):
            with self.subTest(path=path):
                self.assertFalse(ci_scope.content_path(path))


class RawChangesTests(unittest.TestCase):
    def test_nul_records_preserve_newlines_spaces_and_non_utf8_paths(self):
        header = b":100644 100644 " + b"a" * 40 + b" " + b"b" * 40 + b" M\0"
        paths = (b"docs/with spaces.md", b"docs/with\nnewline.md", b"docs/invalid-\xff.md")
        changes = ci_scope.parse_changes(b"".join(header + path + b"\0" for path in paths))
        self.assertEqual(tuple(os.fsencode(change.path) for change in changes), paths)
        self.assertFalse(ci_scope.requires_full(changes))

    def test_truncated_and_malformed_diff_records_refuse(self):
        header = b":100644 100644 " + b"a" * 40 + b" " + b"b" * 40 + b" M\0"
        for raw in (b"unexpected\0README.md\0", header, header + b"README.md", header + b"\0"):
            with self.subTest(raw=raw):
                with self.assertRaises(ci_scope.ScopeError):
                    ci_scope.parse_changes(raw)

    def test_manual_and_unrecognized_events_cannot_select_light(self):
        for event in ("workflow_dispatch", "unrecognized"):
            with self.subTest(event=event), patch.object(ci_scope, "git_output") as git:
                self.assertTrue(ci_scope.select_full(repository=Path.cwd(), event=event, base="", head=""))
                git.assert_not_called()

    def test_bad_commit_identity_and_git_failures_refuse(self):
        for base in ("", "HEAD", "0" * 40, "-unexpected", "a" * 39):
            with self.subTest(base=base), self.assertRaises(ci_scope.ScopeError):
                ci_scope.select_full(repository=Path.cwd(), event="push", base=base, head="b" * 40)
        for error in (OSError("no Git"), subprocess.TimeoutExpired(["git"], 30)):
            with self.subTest(error=error), patch.object(ci_scope.subprocess, "run", side_effect=error):
                with self.assertRaises(ci_scope.ScopeError):
                    ci_scope.git_output(Path.cwd(), arguments=("status",))


class WorkflowSelectionTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(__file__).resolve().parent.parent
        self.workflow = (self.root / ".github/workflows/ci.yml").read_text()

    def test_required_linux_guard_executes_and_refuses_failed_or_invalid_selection(self):
        gate = job_text(self.workflow, "gate-linux")
        self.assertIn("if: ${{ always() }}", gate.split("    steps:", 1)[0])
        guard = steps(gate)["Require a valid check selection"]
        self.assertIn("SELECTION_RESULT: ${{ needs.changes.result }}", guard)
        self.assertIn("FULL: ${{ needs.changes.outputs.full }}", guard)
        script = "\n".join(line[10:] for line in guard.split("run: |\n", 1)[1].splitlines())
        cases = (("success", "true", 0), ("success", "false", 0), ("success", "", 1),
                 ("success", "invalid", 1), ("failure", "false", 1), ("skipped", "true", 1),
                 ("cancelled", "true", 1), ("", "false", 1))
        for result, full, expected in cases:
            with self.subTest(result=result, full=full):
                observed = subprocess.run(
                    ["bash", "-euo", "pipefail", "-c", script], capture_output=True,
                    env={**os.environ, "SELECTION_RESULT": result, "FULL": full},
                    text=True, check=False, timeout=10)
                self.assertEqual(observed.returncode, expected, observed.stderr)

    def test_light_gate_runs_content_checks_and_full_only_steps_have_conditions(self):
        gate_steps = steps(job_text(self.workflow, "gate-linux"))
        light = gate_steps["Check documentation, skills and diagrams"]
        self.assertIn("needs.changes.outputs.full == 'false'", light)
        self.assertIn("run: make check-docs", light)
        full_steps = (
            "Check the JavaScript collector on Node 18", "Install tools", "Run gate checks",
            "Prove selected native lifetime backend", "Run protocol acceptance",
            "Install pinned supervisor test binaries", "Prove private supervisor bootstrap through delegate CLI",
            "Prove external inspection handoff through delegate CLI",
            "Prove private supervisor upgrade recovery through delegate CLI",
            "Prove default user and budget stops through delegate CLI",
            "Prove managed workspace fixtures through delegate CLI", "Run supervisor acceptance",
        )
        for name in full_steps:
            with self.subTest(step=name):
                self.assertIn("needs.changes.outputs.full == 'true'", gate_steps[name])

    def test_pr_and_main_share_one_selector_and_ccr_has_no_duplicate_auto_triggers(self):
        selector = job_text(self.workflow, "changes")
        self.assertIn("fetch-depth: 0", selector)
        for value in ("github.event.pull_request.base.sha || github.event.before",
                      "github.event.pull_request.head.sha || github.event.after",
                      'scripts/ci_scope.py --event "$CHANGE_EVENT"'):
            self.assertIn(value, selector)
        ccr = job_text(self.workflow, "ccr")
        self.assertIn("needs.changes.outputs.full == 'true'", ccr)
        self.assertIn("uses: ./.github/workflows/ccr-integration.yml", ccr)
        triggers = (self.root / ".github/workflows/ccr-integration.yml").read_text().split("permissions:", 1)[0]
        self.assertIn("  workflow_call:", triggers)
        self.assertIn("  workflow_dispatch:", triggers)
        self.assertNotIn("  push:", triggers)
        self.assertNotIn("  pull_request:", triggers)
        self.assertNotIn("paths:", self.workflow.split("jobs:", 1)[0])


class GitRangeTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="delegate-ci-scope-")
        self.addCleanup(directory.cleanup)
        self.repository = Path(directory.name)
        self.environment = {
            **os.environ, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_AUTHOR_NAME": "CI Test", "GIT_AUTHOR_EMAIL": "ci@example.invalid",
            "GIT_COMMITTER_NAME": "CI Test", "GIT_COMMITTER_EMAIL": "ci@example.invalid",
        }
        self.git("init", "--quiet")
        self.write("README.md", "initial\n")
        self.base = self.commit()

    def git(self, *arguments):
        result = subprocess.run(["git", *arguments], cwd=self.repository, env=self.environment,
                                capture_output=True, text=True, check=False, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def write(self, path, text):
        target = self.repository / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)

    def commit(self):
        self.git("add", "--all")
        self.git("commit", "--quiet", "-m", "fixture")
        return self.git("rev-parse", "HEAD")

    def full(self, *, base=None, event="pull_request"):
        return ci_scope.select_full(repository=self.repository, event=event,
                                    base=self.base if base is None else base,
                                    head=self.git("rev-parse", "HEAD"))

    def test_content_only_push_and_pr_are_light_but_code_then_docs_is_full(self):
        self.write("docs/guide.md", "guide\n")
        self.commit()
        self.assertFalse(self.full())
        self.assertFalse(self.full(event="push"))
        self.write("implementation.go", "package main\n")
        self.commit()
        self.write("README.md", "latest commit is only docs\n")
        self.commit()
        self.assertTrue(self.full())
        self.assertTrue(self.full(event="push"))

    def test_merge_base_does_not_misclassify_changes_exclusive_to_newer_base(self):
        original = self.base
        self.git("checkout", "--quiet", "-b", "base-side")
        self.write("base-only.go", "package base\n")
        newer_base = self.commit()
        self.git("checkout", "--quiet", "--detach", original)
        self.write("docs/guide.md", "worker guide\n")
        self.commit()
        self.assertFalse(self.full(base=newer_base))

    def test_deletions_and_both_sides_of_rename_remain_in_scope(self):
        self.write("code.go", "package code\n")
        self.base = self.commit()
        (self.repository / "docs").mkdir()
        self.git("mv", "code.go", "docs/disguised.md")
        self.commit()
        self.assertTrue(self.full())
        self.base = self.git("rev-parse", "HEAD")
        self.git("mv", "docs/disguised.md", "docs/renamed.md")
        self.commit()
        self.assertFalse(self.full())
        self.base = self.git("rev-parse", "HEAD")
        self.git("rm", "docs/renamed.md")
        self.commit()
        self.assertFalse(self.full())

    def test_executable_and_symlink_changes_cannot_be_content_only(self):
        self.git("update-index", "--chmod=+x", "README.md")
        self.git("commit", "--quiet", "-m", "mode change")
        self.assertTrue(self.full())
        if not hasattr(os, "symlink"):
            return
        self.write("docs/guide.md", "guide\n")
        self.base = self.commit()
        (self.repository / "docs/guide.md").unlink()
        try:
            (self.repository / "docs/guide.md").symlink_to("../README.md")
        except OSError as error:
            self.skipTest(f"native symlink creation unavailable: {error}")
        self.commit()
        self.assertTrue(self.full())

    def test_missing_history_fails_and_cli_never_emits_a_safe_flag(self):
        output = self.repository / "scope-output"
        script = Path(ci_scope.__file__).resolve()
        result = subprocess.run(
            [sys.executable, str(script), "--event", "push", "--base", "f" * 40,
             "--head", self.base, "--repository", str(self.repository), "--output", str(output)],
            env=self.environment, capture_output=True, text=True, check=False, timeout=15)
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("CI scope selection failed", result.stderr)
        self.assertFalse(output.exists())

    def test_cli_manual_outputs_full_and_content_push_outputs_light(self):
        self.write("docs/guide.md", "guide\n")
        head = self.commit()
        script = Path(ci_scope.__file__).resolve()
        for event, expected in (("workflow_dispatch", "true"), ("push", "false")):
            with self.subTest(event=event):
                output = self.repository / f"scope-{event}"
                result = subprocess.run(
                    [sys.executable, str(script), "--event", event, "--base", self.base,
                     "--head", head, "--repository", str(self.repository), "--output", str(output)],
                    env=self.environment, capture_output=True, text=True, check=False, timeout=15)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(output.read_text(), f"full={expected}\n")


if __name__ == "__main__":
    unittest.main()
