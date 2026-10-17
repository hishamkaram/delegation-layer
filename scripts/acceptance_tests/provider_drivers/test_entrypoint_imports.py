"""Verify flat and package entrypoints share canonical driver classes."""

from __future__ import annotations

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class EntrypointImportTests(unittest.TestCase):
    def test_protocol_constants_do_not_observe_home_or_resolve_paths(self):
        repository_root = Path(__file__).resolve().parents[3]
        code = "\n".join((
            "import importlib, sys",
            "from pathlib import Path",
            "from unittest.mock import patch",
            "sys.path.insert(0, sys.argv[1])",
            "with patch.object(Path, 'home', side_effect=AssertionError('home observation')), patch.object(Path, 'resolve', side_effect=AssertionError('filesystem observation')):",
            "    for provider in ('agy', 'claude', 'codex'):",
            "        importlib.import_module('scripts.delegate_acceptance.provider_drivers.' + provider + '.constants')",
            "    importlib.import_module('scripts.delegate_acceptance.private_cli.configuration')",
        ))
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run(
                [sys.executable, "-c", code, str(repository_root)], cwd=directory,
                capture_output=True, text=True, timeout=10, check=False,
            )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_flat_and_module_entrypoints_share_canonical_classes(self):
        repository_root = Path(__file__).resolve().parents[3]
        code = '''
import importlib
from pathlib import Path
import runpy
import sys

root = Path(sys.argv[1]).resolve()
sys.path = [item for item in sys.path if Path(item or ".").resolve() != root]
assert all(Path(item or ".").resolve() != root for item in sys.path)
entries = (
    ("acceptance_agy.py", "agy", "NativeRun"),
    ("acceptance_claude.py", "claude", "ClaudeAcceptance"),
    ("acceptance_codex.py", "codex", "CodexAcceptance"),
)
for entry, provider, class_name in entries:
    flat = runpy.run_path(str(root / "scripts" / entry), run_name="flat_entry")
    runner_name = f"scripts.delegate_acceptance.provider_drivers.{provider}.runner"
    runner = importlib.import_module(runner_name)
    module_name = "scripts." + Path(entry).stem
    module = runpy.run_module(module_name, run_name="module_entry")
    assert flat[class_name] is runner.__dict__[class_name]
    assert module[class_name] is runner.__dict__[class_name]
    assert runner.DirectoryOwner is importlib.import_module(
        "scripts.acceptance_supervisor_common").DirectoryOwner
    assert runner.AcceptanceFailure is importlib.import_module(
        "scripts.acceptance_provider_common").AcceptanceFailure
'''
        environment = os.environ.copy()
        environment.pop("PYTHONPATH", None)
        environment["PYTHONNOUSERSITE"] = "1"
        with tempfile.TemporaryDirectory(prefix="acceptance-entry-empty-cwd-") as directory:
            result = subprocess.run(
                [sys.executable, "-c", code, str(repository_root)],
                cwd=directory,
                env=environment,
                capture_output=True,
                text=True,
                timeout=30,
                check=False,
            )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
