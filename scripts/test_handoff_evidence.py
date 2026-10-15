"""Focused, hermetic tests for the handoff evidence index."""

from __future__ import annotations

import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from scripts import handoff_evidence as evidence


class HandoffEvidenceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="handoff-evidence-")
        self.base = Path(self.temporary.name)
        self.repo = self.base / "repo"
        self.repo.mkdir()
        self.output_dir = self.base / "output"
        self.output_dir.mkdir()
        self._git("init", "-q")
        self._git("config", "user.name", "Evidence Test")
        self._git("config", "user.email", "evidence@example.invalid")
        (self.repo / ".gitignore").write_text("ignored-*.txt\n", encoding="utf-8")
        (self.repo / "tracked.txt").write_bytes(b"original\n")
        (self.repo / "old-name.txt").write_bytes(b"rename content\n")
        (self.repo / "delete-me.txt").write_bytes(b"delete content\n")
        self._git("add", ".")
        self._git("commit", "-qm", "baseline")

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def _git(self, *arguments: str) -> str:
        result = subprocess.run(
            ["git", *arguments], cwd=self.repo, check=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        return result.stdout

    def _run(self, output: Path, *arguments: str) -> tuple[int, str]:
        stderr = io.StringIO()
        argv = ["--root", os.fspath(self.repo), "--output", os.fspath(output), *arguments]
        with contextlib.redirect_stderr(stderr):
            code = evidence.main(argv)
        return code, stderr.getvalue()

    @staticmethod
    def _load(path: Path) -> dict:
        return json.loads(path.read_text(encoding="utf-8"))

    def test_nul_safe_manifest_covers_modified_untracked_rename_delete_and_ignored(self) -> None:
        (self.repo / "tracked.txt").write_bytes(b"changed\n")
        os.replace(self.repo / "old-name.txt", self.repo / "renamed.txt")
        (self.repo / "delete-me.txt").unlink()
        (self.repo / "line\nbreak.txt").write_bytes(b"newline path\n")
        (self.repo / "ignored-private.txt").write_bytes(b"ignored\n")
        self._git("add", "--", "tracked.txt", "renamed.txt")
        self._git("rm", "-q", "--", "delete-me.txt")
        output = self.output_dir / "manifest.json"

        code, _ = self._run(output)

        self.assertEqual(code, 0)
        rows = {row["path"]: row for row in self._load(output)["source_manifest"]}
        self.assertEqual(rows["tracked.txt"]["status"], "modified")
        self.assertEqual(rows["line\nbreak.txt"]["status"], "untracked")
        self.assertEqual(rows["renamed.txt"]["status"], "renamed")
        self.assertEqual(rows["renamed.txt"]["renamed_from"], "old-name.txt")
        self.assertEqual(rows["old-name.txt"]["status"], "deleted")
        self.assertIsNone(rows["old-name.txt"]["sha256"])
        self.assertEqual(rows["delete-me.txt"]["status"], "deleted")
        self.assertNotIn("ignored-private.txt", rows)
        self.assertEqual(rows["tracked.txt"]["sha256"], hashlib.sha256(b"changed\n").hexdigest())

    def test_explicit_binaries_and_receipts_are_hashed_and_ordered_deterministically(self) -> None:
        (self.repo / "z-source.txt").write_bytes(b"z")
        (self.repo / "a-source.txt").write_bytes(b"a")
        binary_a, binary_z = self.base / "binary-a", self.base / "binary-z"
        receipt_a, receipt_z = self.base / "receipt-a.json", self.base / "receipt-z.json"
        binary_a.write_bytes(b"binary a")
        binary_z.write_bytes(b"binary z")
        receipt_a.write_bytes(b'{"receipt":"a"}')
        receipt_z.write_bytes(b'{"receipt":"z"}')
        arguments = (
            "--binary", "z-worker", os.fspath(binary_z), "delegate 1.2.3 build-z",
            "--binary", "a-worker", os.fspath(binary_a), "delegate 1.2.3 build-a",
            "--receipt", "native", os.fspath(receipt_z),
            "--receipt", "fixture", os.fspath(receipt_a),
            "--transition-row", "Z-ROW", "--transition-row", "A-ROW",
        )
        first, second = self.output_dir / "first.json", self.output_dir / "second.json"

        self.assertEqual(self._run(first, *arguments)[0], 0)
        self.assertEqual(self._run(second, *arguments)[0], 0)

        self.assertEqual(first.read_bytes(), second.read_bytes())
        document = self._load(first)
        paths = [row["path"] for row in document["source_manifest"]]
        self.assertEqual(paths, sorted(paths, key=os.fsencode))
        source = {row["path"]: row for row in document["source_manifest"]}
        self.assertEqual(source["a-source.txt"]["sha256"], hashlib.sha256(b"a").hexdigest())
        self.assertEqual([row["role"] for row in document["binaries"]], ["a-worker", "z-worker"])
        self.assertEqual(document["binaries"][0]["identity"], "delegate 1.2.3 build-a")
        self.assertEqual(document["binaries"][0]["sha256"], hashlib.sha256(b"binary a").hexdigest())
        self.assertEqual([row["claim_class"] for row in document["receipts"]], ["fixture", "native"])
        self.assertEqual(document["receipts"][0]["sha256"], hashlib.sha256(receipt_a.read_bytes()).hexdigest())
        self.assertEqual(document["transition_rows"], ["A-ROW", "Z-ROW"])

    def test_missing_required_evidence_is_failed_and_unresolved_is_incomplete(self) -> None:
        missing = self.output_dir / "missing.json"
        code, _ = self._run(
            missing,
            "--require-source", "missing.go",
            "--require-binary", "delegate",
            "--require-receipt", "native",
        )
        self.assertEqual(code, 1)
        document = self._load(missing)
        self.assertEqual(document["verdict"], "failed")
        self.assertEqual(document["failure"]["code"], "required_input_missing")
        self.assertEqual(document["missing_required"]["source_paths"], ["missing.go"])

        unresolved = self.output_dir / "unresolved.json"
        code, _ = self._run(unresolved, "--unresolved", "native macOS test pending")
        self.assertEqual(code, 2)
        self.assertEqual(self._load(unresolved)["verdict"], "incomplete")

    def test_repository_without_head_still_indexes_untracked_sources(self) -> None:
        unborn = self.base / "unborn"
        unborn.mkdir()
        subprocess.run(["git", "init", "-q"], cwd=unborn, check=True)
        (unborn / "first.txt").write_bytes(b"first source")
        (unborn / "staged.txt").write_bytes(b"staged source")
        subprocess.run(["git", "add", "staged.txt"], cwd=unborn, check=True)
        output = self.output_dir / "unborn.json"
        stderr = io.StringIO()

        with contextlib.redirect_stderr(stderr):
            code = evidence.main(["--root", os.fspath(unborn), "--output", os.fspath(output)])

        self.assertEqual(code, 0)
        document = self._load(output)
        self.assertIsNone(document["repository"]["head"])
        self.assertIsNone(document["repository"]["baseline"])
        rows = {row["path"]: row for row in document["source_manifest"]}
        self.assertEqual(rows["first.txt"]["status"], "untracked")
        self.assertEqual(rows["staged.txt"]["status"], "added")

    def test_duplicate_or_sensitive_caller_input_finalizes_sanitized_failure(self) -> None:
        binary = self.base / "delegate"
        binary.write_bytes(b"binary")
        duplicate = self.output_dir / "duplicate.json"
        code, _ = self._run(
            duplicate,
            "--binary", "delegate", os.fspath(binary), "build-a",
            "--binary", "delegate", os.fspath(binary), "build-b",
        )
        self.assertEqual(code, 1)
        self.assertEqual(self._load(duplicate)["failure"]["code"], "duplicate_binary_role")

        overlap = self.base / "output-is-input.bin"
        overlap.write_bytes(b"do not replace this binary")
        code, _ = self._run(overlap, "--binary", "delegate", os.fspath(overlap), "build-a")
        self.assertEqual(code, 1)
        self.assertEqual(overlap.read_bytes(), b"do not replace this binary")

        duplicate_path = self.output_dir / "duplicate-path.json"
        code, _ = self._run(
            duplicate_path,
            "--binary", "delegate-a", os.fspath(binary), "build-a",
            "--binary", "delegate-b", os.fspath(binary), "build-b",
        )
        self.assertEqual(code, 1)
        self.assertEqual(self._load(duplicate_path)["failure"]["code"], "duplicate_input_path")

        target, link = self.base / "target", self.base / "binary-link"
        target.write_bytes(b"target bytes")
        link.symlink_to(target)
        symlink_output = self.output_dir / "symlink.json"
        code, _ = self._run(symlink_output, "--binary", "delegate", os.fspath(link), "build")
        self.assertEqual(code, 1)
        self.assertEqual(self._load(symlink_output)["failure"]["code"], "input_not_regular")

        malformed = self.output_dir / "malformed.json"
        code, _ = self._run(malformed, "--transition-row", "not a row id")
        self.assertEqual(code, 1)
        self.assertEqual(self._load(malformed)["failure"]["code"], "transition_row_invalid")

        bad_claim = self.output_dir / "bad-claim.json"
        code, _ = self._run(bad_claim, "--require-receipt", "unknown")
        self.assertEqual(code, 1)
        self.assertEqual(self._load(bad_claim)["failure"]["code"], "claim_class_invalid")

        secret = "token=DO_NOT_LEAK_THIS_VALUE"
        failed = self.output_dir / "sensitive.json"
        with patch.dict(os.environ, {"GH_TOKEN": secret}):
            code, stderr = self._run(failed, "--unresolved", secret)
        self.assertEqual(code, 1)
        self.assertNotIn(secret, stderr)
        self.assertNotIn(secret, failed.read_text(encoding="utf-8"))
        self.assertEqual(self._load(failed)["failure"]["code"], "sensitive_input")

    def test_atomic_output_replacement_and_failed_replace_preserves_previous_file(self) -> None:
        output = self.output_dir / "index.json"
        output.write_text("previous\n", encoding="utf-8")
        self.assertEqual(self._run(output, "--transition-row", "SUPERVISOR-01")[0], 0)
        previous = output.read_bytes()
        self.assertNotEqual(previous, b"previous\n")

        with patch.object(evidence.os, "replace", side_effect=OSError("private detail")):
            with self.assertRaises(evidence.EvidenceError) as caught:
                evidence._atomic_write(output, {"schema_version": 1})
        self.assertEqual(caught.exception.code, "output_write_failed")
        self.assertEqual(output.read_bytes(), previous)
        self.assertEqual(list(self.output_dir.glob(".index.json.*.tmp")), [])


if __name__ == "__main__":
    unittest.main()
