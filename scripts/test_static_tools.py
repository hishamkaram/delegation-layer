"""Static tooling must preserve explicit coverage, failure statuses, and pins."""

from pathlib import Path
import os
import subprocess
import tempfile
import unittest
from unittest import mock

from scripts import static_analysis as analysis
from scripts import static_tools as tools
from scripts import verify_static_gates as proofs


class StaticAnalysisTests(unittest.TestCase):
    def test_inventory_preserves_hidden_ancestor_and_spaces(self):
        root = Path("/.hidden/project directory")
        expected = [str(root / "a file.py"), str(root / "z.py")]
        output = b"\0".join(path.encode() for path in expected) + b"\0"
        with mock.patch.object(analysis.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, output)) as run:
            self.assertEqual(analysis.source_paths("python", root=root), expected)
        run.assert_called_once_with(["go", "run", "./cmd/source-inventory", "--kind", "python"],
                                    cwd=root, capture_output=True, check=False, timeout=120)

    def test_empty_or_malformed_inventory_is_not_a_clean_scan(self):
        for output in (b"", b"\0", b"/repo/a.py", b"/repo/b.py\0/repo/a.py\0", b"/repo/a.py\0/repo/a.py\0"):
            with self.subTest(output=output), mock.patch.object(
                    analysis.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, output)):
                with self.assertRaises(ValueError):
                    analysis.source_paths("python", root=Path("/repo"))

    def test_inventory_failure_preserves_diagnostic(self):
        failed = subprocess.CompletedProcess([], 2, b"", b"no maintained python sources\n")
        with mock.patch.object(analysis.subprocess, "run", return_value=failed):
            with self.assertRaisesRegex(ValueError, "exit 2.*no maintained python sources"):
                analysis.source_paths("python", root=Path("/repo"))

    def test_wrong_extension_or_external_path_is_rejected(self):
        for output in (b"/repo/file.sh\0", b"/outside/file.py\0"):
            with self.subTest(output=output), mock.patch.object(
                    analysis.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, output)):
                with self.assertRaises(ValueError):
                    analysis.source_paths("python", root=Path("/repo"))

    def test_batching_keeps_every_argument_once(self):
        paths = ["a file.py", "b.py", "c.py"]
        batches = analysis.batches(paths, byte_limit=12)
        self.assertEqual([path for batch in batches for path in batch], paths)
        self.assertEqual(batches, [["a file.py"], ["b.py", "c.py"]])
        with self.assertRaises(ValueError):
            analysis.batches(["long-file.py"], byte_limit=4)

    def test_later_batch_failure_is_not_hidden(self):
        with mock.patch.object(analysis, "source_paths", return_value=["a.py", "b.py"]), mock.patch.object(
                analysis, "batches", return_value=[["a.py"], ["b.py"]]), mock.patch.object(
                analysis, "execute", side_effect=[0, 2]) as execute:
            self.assertEqual(analysis.run_check("python"), 2)
            self.assertEqual(execute.call_count, 2)

    def test_workflow_scope_has_no_suppression(self):
        command, language = analysis.commands("workflows")
        self.assertIsNone(language)
        self.assertNotIn("-ignore", command)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            workflows = root / ".github/workflows"
            workflows.mkdir(parents=True)
            names = ("ci.yml", "ccr-integration.yml", "future.yaml", "release.yml", "notes.md")
            for name in names:
                (workflows / name).touch()
            with mock.patch.object(analysis, "ROOT", root):
                self.assertEqual(analysis.workflow_paths(), [
                    str(workflows / name) for name in ("ccr-integration.yml", "ci.yml", "future.yaml", "release.yml")])
        self.assertIn("--no-ignores", command)
        self.assertIn("--no-baseline", command)

    def test_empty_workflow_scope_is_not_a_clean_scan(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / ".github/workflows").mkdir(parents=True)
            with mock.patch.object(analysis, "ROOT", root):
                with self.assertRaisesRegex(ValueError, "no supported workflow"):
                    analysis.workflow_paths()

    def test_required_gate_runs_type_and_security_checks_and_preserves_failure(self):
        with mock.patch.object(analysis.sys, "argv", ["static_analysis.py", "all"]), mock.patch.object(
                analysis, "run_check", side_effect=[0, 0, 0, 0, 1, 0]) as run:
            self.assertEqual(analysis.main(), 1)
        self.assertEqual([call.args[0] for call in run.call_args_list],
                         ["python", "javascript", "shell", "workflows", "ty", "zizmor"])

    def test_proofs_use_the_same_commands_as_the_gate(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(proofs, "prove") as prove:
            proofs.lint_fixtures(Path(directory))
            checks = {"Ruff": "python", "ShellCheck": "shell", "ESLint": "javascript", "jactionlint": "workflows"}
            for call in prove.call_args_list:
                self.assertEqual(call.args[1], analysis.commands(checks[call.args[0].split()[0]])[0])
            prove.reset_mock()
            proofs.type_fixtures(Path(directory))
            self.assertEqual(prove.call_count, 2)
            for call in prove.call_args_list:
                self.assertEqual(call.args[1], analysis.commands("ty")[0])
            prove.reset_mock()
            proofs.security_fixtures(Path(directory))
            self.assertEqual(prove.call_count, 1)
            self.assertEqual(prove.call_args.args[1], analysis.commands("zizmor")[0])

    def test_killed_analyzer_is_not_a_clean_scan(self):
        with mock.patch.object(analysis.subprocess, "run", return_value=subprocess.CompletedProcess([], -9)):
            with self.assertRaisesRegex(ValueError, "signal 9"):
                analysis.execute(["fixture-analyzer"])


class StaticToolTests(unittest.TestCase):
    def test_default_verification_requires_type_and_security_tool_versions(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(tools, "ROOT", Path(directory)), \
                mock.patch.object(tools.sys, "argv", ["static_tools.py", "verify"]), \
                mock.patch.object(tools, "python_tools") as python, \
                mock.patch.object(tools, "node_tools"), mock.patch.object(tools, "require_banner"):
            self.assertEqual(tools.main(), 0)
        self.assertEqual(python.call_args_list,
                         [mock.call("lint", install=False), mock.call("diagnostics", install=False)])

    def test_shellcheck_has_all_supported_targets(self):
        for system in ("Linux", "Darwin"):
            for machine in ("x86_64", "aarch64"):
                name, digest = tools.shellcheck_target(system, machine)
                self.assertIn(system.lower(), name)
                self.assertEqual(len(digest), 64)
                int(digest, 16)
        with self.assertRaises(ValueError):
            tools.shellcheck_target("Windows", "x86_64")

    def test_exact_pin_required(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "requirements.txt"
            path.write_text("ruff==0.15.20 \\\n    --hash=sha256:123\n")
            self.assertEqual(tools.requirement_version(path, "ruff"), "0.15.20")
            path.write_text("ruff>=0.15.20\n")
            with self.assertRaises(ValueError):
                tools.requirement_version(path, "ruff")

    def test_banner_mismatch_and_tool_failure_are_rejected(self):
        with mock.patch.object(tools.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "ruff 0.15.200\n")):
            with self.assertRaisesRegex(ValueError, "received 'ruff 0.15.200'"):
                tools.require_banner(["ruff", "--version"], "ruff 0.15.20")
        with mock.patch.object(tools.subprocess, "run", side_effect=subprocess.CalledProcessError(2, "ruff")):
            with self.assertRaises(subprocess.CalledProcessError):
                tools.require_banner(["ruff", "--version"], "ruff 0.15.20")

    def test_version_mismatch_diagnostic_is_bounded_and_escaped(self):
        for stdout, stderr in (("x" * 500, ""), ("", "wrong\x1b[31m\nbanner")):
            with self.subTest(stdout=bool(stdout)), mock.patch.object(
                    tools.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, stdout, stderr)):
                with self.assertRaises(ValueError) as raised:
                    tools.require_banner(["ruff", "--version"], "ruff 0.15.20")
                observed = (stdout + stderr).strip()[:200]
                self.assertIn(f"received {observed!r}", str(raised.exception))
                self.assertNotIn("x" * 201, str(raised.exception))
                self.assertNotIn("\x1b", str(raised.exception))

    def test_ty_version_accepts_build_metadata_but_requires_exact_version(self):
        for payload in ('{"version":"0.0.97","commit_info":null}',
                        '{"version":"0.0.97","commit_info":{"short":"58e168c67","date":"2026-11-04"}}'):
            with self.subTest(payload=payload), mock.patch.object(
                    tools.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, payload)) as run:
                tools.require_ty_version(Path("/tools/ty"), "0.0.97")
                self.assertEqual(run.call_args.args[0], ["/tools/ty", "version", "--output-format", "json"])
        for payload in ('{"version":"0.0.970"}', '{"version":"0.0.96"}',
                        '{"commit_info":null}', '[]', 'not JSON'):
            with self.subTest(payload=payload), mock.patch.object(
                    tools.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, payload)):
                with self.assertRaises(ValueError):
                    tools.require_ty_version(Path("/tools/ty"), "0.0.97")

    def test_download_script_does_not_mask_command_substitution_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / "dirname"
            stub.write_text("#!/bin/sh\nexit 71\n")
            stub.chmod(0o755)
            installer = Path(directory) / "python3"
            installer.write_text("#!/bin/sh\nexit 72\n")
            installer.chmod(0o755)
            result = subprocess.run(["bash", str(tools.ROOT / "scripts/fetch-pueue-assets.sh"), directory],
                                    env={**os.environ, "PATH": directory + os.pathsep + os.environ["PATH"]},
                                    capture_output=True, text=True, check=False, timeout=10)
            self.assertEqual(result.returncode, 71, result.stderr)


if __name__ == "__main__":
    unittest.main()
