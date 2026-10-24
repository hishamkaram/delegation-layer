"""Focused, hermetic tests for the handoff evidence index."""

from __future__ import annotations

import contextlib
import errno
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import tracemalloc
import unittest
from unittest.mock import patch

from scripts import handoff_evidence as evidence
from scripts import handoff_index
from scripts.acceptance_tests.evidence.test_bounded_files import injected_at_lstat, without_nofollow
from scripts.delegate_acceptance.evidence import bounded_files


class RepositoryFixture(unittest.TestCase):
    """Temporary baseline repository plus CLI helpers shared by the test cases."""

    temporary_alias = False

    def setUp(self) -> None:
        self.temporary = self._open_temporary()
        # The one boundary that canonicalizes the fixture: the product reports resolved artifact
        # paths, and a platform temp directory may be reached through a symlink alias.
        self.base = Path(self.temporary.name).resolve(strict=True)
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

    def _open_temporary(self) -> tempfile.TemporaryDirectory:
        """Create the fixture through the system temp API, optionally below a symlinked alias."""
        if not self.temporary_alias:
            return tempfile.TemporaryDirectory(prefix="handoff-evidence-")
        container = tempfile.TemporaryDirectory(prefix="handoff-alias-")
        self.addCleanup(container.cleanup)
        real = Path(container.name).resolve(strict=True) / "real"
        alias = real.with_name("alias")
        real.mkdir()
        alias.symlink_to(real, target_is_directory=True)
        with patch.object(tempfile, "tempdir", os.fspath(alias)):
            return tempfile.TemporaryDirectory(prefix="handoff-evidence-")

    def _git(self, *arguments: str) -> str:
        result = subprocess.run(
            ["git", *arguments], cwd=self.repo, check=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        return result.stdout

    def _run(self, output: Path, arguments: tuple[str, ...] = ()) -> tuple[int, str]:
        stderr = io.StringIO()
        argv = ["--root", os.fspath(self.repo), "--output", os.fspath(output), *arguments]
        with contextlib.redirect_stderr(stderr):
            code = evidence.main(argv)
        return code, stderr.getvalue()

    @staticmethod
    def _load(path: Path) -> dict:
        return json.loads(path.read_text(encoding="utf-8"))


class HandoffEvidenceTests(RepositoryFixture):
    def test_run_preserves_each_argv_sequence_element(self) -> None:
        output = self.output_dir / "output with spaces.json"
        arguments = ("--transition-row", "SUPERVISOR-01", "", "a b", "literal*value")
        with patch.object(evidence, "main", return_value=0) as main:
            self.assertEqual(self._run(output, arguments), (0, ""))
        main.assert_called_once_with([
            "--root", os.fspath(self.repo), "--output", os.fspath(output), *arguments,
        ])

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

        self.assertEqual(self._run(first, arguments)[0], 0)
        self.assertEqual(self._run(second, arguments)[0], 0)

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
            ("--require-source", "missing.go",
            "--require-binary", "delegate",
            "--require-receipt", "native"),
        )
        self.assertEqual(code, 1)
        document = self._load(missing)
        self.assertEqual(document["verdict"], "failed")
        self.assertEqual(document["failure"]["code"], "required_input_missing")
        self.assertEqual(document["missing_required"]["source_paths"], ["missing.go"])

        unresolved = self.output_dir / "unresolved.json"
        code, _ = self._run(unresolved, ("--unresolved", "native macOS test pending"))
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
            ("--binary", "delegate", os.fspath(binary), "build-a",
            "--binary", "delegate", os.fspath(binary), "build-b"),
        )
        self.assertEqual(code, 1)
        self.assertEqual(self._load(duplicate)["failure"]["code"], "duplicate_binary_role")

        overlap = self.base / "output-is-input.bin"
        overlap.write_bytes(b"do not replace this binary")
        code, _ = self._run(overlap, ("--binary", "delegate", os.fspath(overlap), "build-a"))
        self.assertEqual(code, 1)
        self.assertEqual(overlap.read_bytes(), b"do not replace this binary")

        duplicate_path = self.output_dir / "duplicate-path.json"
        code, _ = self._run(
            duplicate_path,
            ("--binary", "delegate-a", os.fspath(binary), "build-a",
            "--binary", "delegate-b", os.fspath(binary), "build-b"),
        )
        self.assertEqual(code, 1)
        self.assertEqual(self._load(duplicate_path)["failure"]["code"], "duplicate_input_path")

        target, link = self.base / "target", self.base / "binary-link"
        target.write_bytes(b"target bytes")
        link.symlink_to(target)
        symlink_output = self.output_dir / "symlink.json"
        code, _ = self._run(symlink_output, ("--binary", "delegate", os.fspath(link), "build"))
        self.assertEqual(code, 1)
        self.assertEqual(self._load(symlink_output)["failure"]["code"], "input_not_regular")

        malformed = self.output_dir / "malformed.json"
        code, _ = self._run(malformed, ("--transition-row", "not a row id"))
        self.assertEqual(code, 1)
        self.assertEqual(self._load(malformed)["failure"]["code"], "transition_row_invalid")

        bad_claim = self.output_dir / "bad-claim.json"
        code, _ = self._run(bad_claim, ("--require-receipt", "unknown"))
        self.assertEqual(code, 1)
        self.assertEqual(self._load(bad_claim)["failure"]["code"], "claim_class_invalid")

        secret = "token=DO_NOT_LEAK_THIS_VALUE"
        failed = self.output_dir / "sensitive.json"
        with patch.dict(os.environ, {"GH_TOKEN": secret}):
            code, stderr = self._run(failed, ("--unresolved", secret))
        self.assertEqual(code, 1)
        self.assertNotIn(secret, stderr)
        self.assertNotIn(secret, failed.read_text(encoding="utf-8"))
        self.assertEqual(self._load(failed)["failure"]["code"], "sensitive_input")

    def test_atomic_output_replacement_and_failed_replace_preserves_previous_file(self) -> None:
        output = self.output_dir / "index.json"
        output.write_text("previous\n", encoding="utf-8")
        self.assertEqual(self._run(output, ("--transition-row", "SUPERVISOR-01"))[0], 0)
        previous = output.read_bytes()
        self.assertNotEqual(previous, b"previous\n")

        with patch.object(evidence.os, "replace", side_effect=OSError("private detail")):
            with self.assertRaises(evidence.EvidenceError) as caught:
                evidence._atomic_write(output, {"schema_version": 1})
        self.assertEqual(caught.exception.code, "output_write_failed")
        self.assertEqual(output.read_bytes(), previous)
        self.assertEqual(list(self.output_dir.glob(".index.json.*.tmp")), [])


class HandoffVerifyTests(RepositoryFixture):
    """Read-only verification of an index against the inputs it declares."""

    def _verify(self, index: Path, arguments: tuple[str, ...] = ()) -> tuple[int, dict, str]:
        stdout, stderr = io.StringIO(), io.StringIO()
        argv = ["--root", os.fspath(self.repo), "--verify", os.fspath(index), *arguments]
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = evidence.main(argv)
        return code, json.loads(stdout.getvalue()), stderr.getvalue()

    def _index(self, extra: tuple[str, ...] = (), receipt: bytes = b'{"claim":"fixture"}') -> Path:
        self.binary, self.receipt = self.base / "delegate-bin", self.base / "native-receipt.json"
        self.binary.write_bytes(b"binary v1")
        self.receipt.write_bytes(receipt)
        (self.repo / "tracked.txt").write_bytes(b"changed\n")
        index = self.output_dir / "index.json"
        code, _ = self._run(index, (
            "--binary", "delegate", os.fspath(self.binary), "delegate 1.2.3",
            "--receipt", "fixture", os.fspath(self.receipt), "--transition-row", "SUPERVISOR-01", *extra,
        ))
        self.assertIn(code, (0, 2))
        return index

    def _snapshot(self) -> tuple[dict[str, bytes], str]:
        files = {
            os.fspath(path): path.read_bytes()
            for root in (self.output_dir, self.repo, self.base / "delegate-bin")
            for path in ([root] if root.is_file() else root.rglob("*"))
            if path.is_file() and ".git" not in path.parts
        }
        return files, self._git("status", "--porcelain", "-uall")

    def _mutate_index(self, index: Path, change) -> None:
        document = self._load(index)
        change(document)
        index.write_text(json.dumps(document), encoding="utf-8")

    def test_unchanged_snapshot_verifies_without_mutating_anything(self) -> None:
        index = self._index()
        before = self._snapshot()

        code, report, stderr = self._verify(index)

        self.assertEqual((code, stderr), (0, ""))
        self.assertEqual(report["verified"], True)
        self.assertEqual(report["declared_verdict"], "complete")
        self.assertEqual(report["differences"], [])
        self.assertEqual(self._snapshot(), before)

    def _stale_after(self, change) -> list[dict]:
        index = self._index()
        change()
        code, report, _ = self._verify(index)
        self.assertEqual((code, report["verified"]), (1, False))
        return report["differences"]

    def test_modified_source_is_stale(self) -> None:
        differences = self._stale_after(lambda: (self.repo / "tracked.txt").write_bytes(b"later\n"))
        self.assertEqual(differences, [{"field": "source_manifest", "key": "tracked.txt", "kind": "changed"}])

    def test_new_untracked_source_is_stale(self) -> None:
        differences = self._stale_after(lambda: (self.repo / "new.txt").write_bytes(b"new"))
        self.assertEqual(differences, [{"field": "source_manifest", "key": "new.txt", "kind": "added"}])

    def test_deleted_source_is_stale(self) -> None:
        differences = self._stale_after(lambda: (self.repo / "delete-me.txt").unlink())
        self.assertEqual(differences, [{"field": "source_manifest", "key": "delete-me.txt", "kind": "added"}])

    def test_moved_head_is_stale_but_branch_and_checkout_name_are_informational(self) -> None:
        index = self._index()
        self._git("checkout", "-q", "-b", "renamed-branch")
        self.assertEqual(self._verify(index)[0], 0)

        self._git("add", ".")
        self._git("commit", "-qm", "move head")
        code, report, _ = self._verify(index)

        self.assertEqual(code, 1)
        self.assertEqual(report["differences"], [{"field": "repository.head", "kind": "changed"}])

    def test_same_identity_with_different_binary_or_receipt_bytes_is_stale(self) -> None:
        index = self._index()
        self.binary.write_bytes(b"binary v2")
        self.receipt.write_bytes(b'{"claim":"other"}')

        code, report, _ = self._verify(index)

        self.assertEqual(code, 1)
        self.assertIn({"field": "binaries", "key": "delegate", "kind": "changed"}, report["differences"])
        self.assertIn({"field": "receipts", "key": f"fixture:{self.receipt}", "kind": "changed"}, report["differences"])
        self.assertEqual(self._load(index)["binaries"][0]["identity"], "delegate 1.2.3")

    def test_missing_symlinked_and_oversized_artifacts_fail_with_a_stable_code(self) -> None:
        index = self._index()
        with patch.object(handoff_index, "MAX_FILE_BYTES", 4):
            self.assertEqual(self._verify(index)[1]["failure"]["code"], "input_too_large")
        target = self.base / "link-target"
        target.write_bytes(b"binary v1")
        self.binary.unlink()
        self.binary.symlink_to(target)
        self.assertEqual(self._verify(index)[1]["failure"]["code"], "input_not_regular")
        self.binary.unlink()
        code, report, stderr = self._verify(index)
        self.assertEqual((code, report["verified"], report["failure"]["code"]), (1, False, "input_unavailable"))
        self.assertEqual(stderr, "handoff evidence: input_unavailable\n")

    def test_complete_is_declared_input_completeness_not_receipt_interpretation(self) -> None:
        index = self._index(receipt=b'{"verdict":"failed","native":"FAIL"}')

        code, report, _ = self._verify(index)

        self.assertEqual((code, report["declared_verdict"], report["verified"]), (0, "complete", True))

    def test_declared_unresolved_work_verifies_as_incomplete_and_cannot_be_relabelled(self) -> None:
        index = self._index(extra=("--unresolved", "native macOS pending"))
        code, report, _ = self._verify(index)
        self.assertEqual((code, report["declared_verdict"], report["verified"]), (2, "incomplete", True))

        self._mutate_index(index, lambda document: document.update(verdict="complete"))
        code, report, _ = self._verify(index)
        self.assertEqual(code, 1)
        self.assertIn({"field": "verdict", "kind": "changed"}, report["differences"])

    def _ignore_outputs(self) -> None:
        """Commit ignore rules for in-repository output, plus a tracked file a glob would also match."""
        (self.repo / ".gitignore").write_text("ignored-*.txt\n.local/\na\\[b\\].json\n", encoding="utf-8")
        (self.repo / "ab.json").write_bytes(b"tracked glob decoy\n")
        self._git("add", "--", ".gitignore", "ab.json")
        self._git("commit", "-qm", "ignore handoff output")

    def _source_state(self) -> tuple[str, bytes, dict[str, bytes], str]:
        tracked = {name: (self.repo / name).read_bytes() for name in ("tracked.txt", "ab.json", ".gitignore")}
        return (self._git("rev-parse", "HEAD"), (self.repo / ".git" / "index").read_bytes(), tracked,
                self._git("status", "--porcelain", "-uall"))

    def test_ignored_output_inside_the_repository_generates_and_verifies_read_only(self) -> None:
        self._ignore_outputs()
        cases = (
            (".local/archive/probe.json", (), 0),
            (".local/archive[1]/odd*?[x].json", ("--unresolved", "native macOS pending"), 2),
            ("a[b].json", (), 0),
        )
        for relative, extra, expected in cases:
            with self.subTest(output=relative):
                output = self.repo / relative
                before = self._source_state()
                self.assertEqual(self._run(output, extra)[0], expected)
                self.assertEqual(self._source_state(), before)
                code, report, stderr = self._verify(output)
                self.assertEqual((code, report["verified"], report["differences"], stderr), (expected, True, [], ""))
                self.assertEqual(self._source_state(), before)

    def test_inherited_pathspec_settings_cannot_break_generation(self) -> None:
        self._ignore_outputs()
        for name in handoff_index.PATHSPEC_ENVIRONMENT:
            with self.subTest(setting=name), patch.dict(os.environ, {name: "1"}):
                output = self.repo / ".local" / "archive" / f"{name}.json"
                self.assertEqual(self._run(output), (0, ""))
                self.assertEqual(self._verify(output)[1]["verified"], True)

    def test_in_repository_output_that_is_tracked_or_not_ignored_is_still_refused(self) -> None:
        self._ignore_outputs()
        tracked, untracked = self.repo / "tracked.txt", self.repo / "new-output.json"
        before = tracked.read_bytes()
        code, stderr = self._run(tracked)
        self.assertEqual((code, stderr), (1, "handoff evidence: output_overlaps_input\n"))
        self.assertEqual(tracked.read_bytes(), before)
        code, stderr = self._run(untracked)
        self.assertEqual((code, stderr), (1, "handoff evidence: output_inside_source_scope\n"))
        self.assertFalse(untracked.exists())

    def test_fatal_git_error_while_validating_output_fails_explicitly_without_writing(self) -> None:
        self._ignore_outputs()
        (self.repo / ".git" / "index").write_bytes(b"not an index")
        output = self.repo / ".local" / "archive" / "probe.json"
        self.assertEqual(self._run(output), (1, "handoff evidence: git_command_failed\n"))
        self.assertFalse(output.exists())

    def test_check_ignore_takes_pathnames_so_pathspec_magic_is_refused_by_git(self) -> None:
        self._ignore_outputs()
        arguments = ("check-ignore", "-q", "--no-index", "--", ".local/archive/probe.json")
        literal = handoff_index.run_git(self.repo, *arguments, stage="test", allow_failure=True)
        plain = handoff_index.run_git(self.repo, *arguments, stage="test", literal_pathspecs=False)
        self.assertEqual((literal[0], plain[0]), (128, 0))
        with self.assertRaises(handoff_index.EvidenceError) as caught:
            handoff_index.run_git(self.repo, *arguments, stage="test")
        self.assertEqual(caught.exception.code, "git_command_failed")

    def _binary_arguments(self, path: Path) -> tuple[str, ...]:
        return ("--binary", "delegate", os.fspath(path), "delegate 1.2.3")

    def test_artifact_replaced_after_observation_is_refused_not_followed(self) -> None:
        artifact, replacement = self.base / "artifact.bin", self.base / "replacement.bin"
        artifact.write_bytes(b"observed")
        replacement.write_bytes(b"swapped in")
        output = self.output_dir / "replaced.json"

        with injected_at_lstat(artifact, lambda: replacement.replace(artifact)) as ran:
            code, stderr = self._run(output, self._binary_arguments(artifact))

        self.assertEqual(ran, [True])
        self.assertEqual((code, stderr), (1, "handoff evidence: input_unavailable\n"))
        self.assertEqual(self._load(output)["failure"]["code"], "input_unavailable")

    def test_alias_retargeted_during_the_read_records_the_selected_canonical_path(self) -> None:
        first, second, alias = self.base / "first", self.base / "second", self.base / "alias"
        for directory, content in ((first, b"first bytes"), (second, b"second bytes")):
            directory.mkdir()
            (directory / "artifact.bin").write_bytes(content)
        alias.symlink_to(first, target_is_directory=True)
        selected = (first / "artifact.bin").stat().st_ino
        original_read, retargeted = os.read, []

        def retarget_while_reading(descriptor: int, length: int) -> bytes:
            if not retargeted and os.fstat(descriptor).st_ino == selected:
                retargeted.append(True)
                alias.unlink()
                alias.symlink_to(second, target_is_directory=True)
            return original_read(descriptor, length)

        output = self.output_dir / "alias.json"
        with patch.object(bounded_files.os, "read", side_effect=retarget_while_reading):
            code, _ = self._run(output, self._binary_arguments(alias / "artifact.bin"))

        self.assertEqual((code, retargeted), (0, [True]))
        (record,) = self._load(output)["binaries"]
        self.assertEqual(record["path"], os.fspath(first / "artifact.bin"))
        self.assertEqual(record["sha256"], hashlib.sha256(b"first bytes").hexdigest())

    def test_fingerprint_streams_a_large_artifact_with_bounded_memory(self) -> None:
        chunk, expected, large = bytes(range(256)) * 4096, hashlib.sha256(), self.base / "large.bin"
        with large.open("wb") as stream:
            for _ in range(16):
                stream.write(chunk)
                expected.update(chunk)
        tracemalloc.start()
        try:
            tracemalloc.reset_peak()
            record = handoff_index._fingerprint(large, "fingerprint-binary", handoff_index.MAX_TOTAL_BYTES)
            peak = tracemalloc.get_traced_memory()[1]
        finally:
            tracemalloc.stop()
        self.assertEqual(record, {"path": os.fspath(large), "size_bytes": 16 * len(chunk), "sha256": expected.hexdigest()})
        self.assertLess(peak, 4 * 1024 * 1024)

    def test_index_replaced_after_observation_is_refused_not_followed(self) -> None:
        index = self._index()
        replacement = self.output_dir / "replacement.json"
        replacement.write_bytes(index.read_bytes())

        with injected_at_lstat(index, lambda: replacement.replace(index)) as ran:
            code, report, _ = self._verify(index)

        self.assertEqual(ran, [True])
        self.assertEqual((code, report["failure"]["code"]), (1, "index_unavailable"))

    def test_index_reads_without_the_optional_nofollow_flag_and_still_refuses_a_swap(self) -> None:
        index = self._index()
        with without_nofollow():
            code, report, _ = self._verify(index)
            self.assertEqual((code, report["verified"]), (0, True))
            target = self.output_dir / "target.json"
            target.write_bytes(index.read_bytes())

            def swap() -> None:
                index.unlink()
                index.symlink_to(target)

            with injected_at_lstat(index, swap) as ran:
                code, report, _ = self._verify(index)
        self.assertEqual(ran, [True])
        self.assertEqual((code, report["failure"]["code"]), (1, "index_unavailable"))

    def _loop_parent(self) -> Path:
        """A directory alias that can never resolve: two symlinks pointing at each other."""
        first, second = self.base / "loop-first", self.base / "loop-second"
        first.symlink_to(second, target_is_directory=True)
        second.symlink_to(first, target_is_directory=True)
        return first

    def _main(self, *arguments: str) -> tuple[int, str, str]:
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = evidence.main(list(arguments))
        return code, stdout.getvalue(), stderr.getvalue()

    def _failure_of(self, output: Path) -> dict:
        self.assertTrue(output.is_file())
        return self._load(output)["failure"]

    def test_parent_symlink_loops_fail_with_the_domain_code_not_an_internal_error(self) -> None:
        loop, environment = self._loop_parent(), dict(os.environ)
        output = self.output_dir / "loop-case.json"

        code, _, stderr = self._main("--root", os.fspath(loop / "repo"), "--output", os.fspath(output))
        self.assertEqual((code, stderr), (1, "handoff evidence: repository_unavailable\n"))
        self.assertFalse(output.exists())
        with self.assertRaises(handoff_index.EvidenceError) as caught:
            handoff_index.repository_root(os.fspath(loop / "repo"))
        self.assertEqual((caught.exception.code, caught.exception.stage), ("repository_unavailable", "resolve-repository"))

        code, stderr = self._run(output, ("--binary", "delegate", os.fspath(loop / "binary"), "build"))
        self.assertEqual((code, stderr), (1, "handoff evidence: input_unavailable\n"))
        self.assertEqual(self._failure_of(output), {"code": "input_unavailable", "stage": "fingerprint-binary"})

        code, stdout, stderr = self._main("--root", os.fspath(self.repo), "--verify", os.fspath(loop / "index.json"))
        self.assertEqual((code, stderr), (1, "handoff evidence: index_unavailable\n"))
        self.assertEqual(json.loads(stdout)["failure"], {"code": "index_unavailable", "stage": "read-index"})

        refused = self.output_dir / "refused.json"
        code, _, stderr = self._main("--root", os.fspath(self.repo), "--output", os.fspath(loop / "refused.json"))
        self.assertEqual((code, stderr), (1, "handoff evidence: invalid_output_path\n"))
        self.assertEqual((list(self.output_dir.iterdir()), refused.exists()), ([output], False))
        self.assertEqual(dict(os.environ), environment)

    def test_tracked_source_symlink_to_itself_is_an_unavailable_source_path(self) -> None:
        (self.repo / "selfloop").symlink_to("selfloop")
        self._git("add", "--", "selfloop")
        output = self.output_dir / "selfloop.json"

        code, stderr = self._run(output)

        self.assertEqual((code, stderr), (1, "handoff evidence: source_path_unavailable\n"))
        self.assertEqual(self._failure_of(output), {"code": "source_path_unavailable", "stage": "fingerprint-source"})

    def test_source_that_becomes_unresolvable_after_fingerprinting_is_an_unavailable_source_path(self) -> None:
        (self.repo / "tracked.txt").write_bytes(b"changed so the manifest fingerprints it\n")
        original_artifacts = handoff_index._artifacts
        output = self.output_dir / "late-loop.json"
        changed = []

        def artifacts_then_loop(*arguments, **options):
            result = original_artifacts(*arguments, **options)
            (self.repo / "tracked.txt").unlink()
            (self.repo / "tracked.txt").symlink_to("tracked.txt")
            changed.append(True)
            return result

        with patch.object(handoff_index, "_artifacts", side_effect=artifacts_then_loop):
            code, stderr = self._run(output)

        self.assertEqual(changed, [True])
        self.assertEqual((code, stderr), (1, "handoff evidence: source_path_unavailable\n"))
        self.assertEqual(self._failure_of(output), {"code": "source_path_unavailable", "stage": "fingerprint-source"})

    def test_new_output_in_an_existing_directory_is_written_and_final_symlinks_stay_refused(self) -> None:
        fresh = self.output_dir / "fresh.json"
        self.assertEqual(self._run(fresh), (0, ""))
        self.assertEqual(self._load(fresh)["verdict"], "complete")

        target = self.base / "leaf-target"
        target.write_bytes(b"target")
        leaf = self.base / "leaf-link"
        leaf.symlink_to(target)
        output = self.output_dir / "leaf.json"
        self.assertEqual(self._run(output, ("--binary", "delegate", os.fspath(leaf), "build"))[0], 1)
        self.assertEqual(self._failure_of(output)["code"], "input_not_regular")
        code, stdout, _ = self._main("--root", os.fspath(self.repo), "--verify", os.fspath(leaf))
        self.assertEqual((code, json.loads(stdout)["failure"]["code"]), (1, "index_not_regular"))
        before = target.read_bytes()
        code, _, stderr = self._main("--root", os.fspath(self.repo), "--output", os.fspath(leaf))
        self.assertEqual((code, stderr), (1, "handoff evidence: invalid_output_path\n"))
        self.assertEqual(target.read_bytes(), before)

    def test_canonical_path_normalizes_only_the_python_312_symlink_loop_error(self) -> None:
        path = self.base / "anything"
        with patch.object(Path, "resolve", side_effect=RuntimeError("Symlink loop from 'x'")):
            with self.assertRaises(OSError) as caught:
                handoff_index.canonical_path(path, strict=True)
        self.assertEqual(caught.exception.errno, errno.ELOOP)
        with patch.object(Path, "resolve", side_effect=FileNotFoundError(errno.ENOENT, "gone")):
            with self.assertRaises(FileNotFoundError):
                handoff_index.canonical_path(path, strict=False)
        with patch.object(Path, "resolve", side_effect=RecursionError):
            with self.assertRaises(RecursionError):
                handoff_index.canonical_path(path, strict=True)
            output = self.output_dir / "recursion.json"
            code, _, stderr = self._main("--root", os.fspath(self.repo), "--output", os.fspath(output))
        self.assertEqual((code, stderr), (1, "handoff evidence: internal_failure\n"))
        self.assertFalse(output.exists())

    def test_unmet_requirements_since_generation_are_reported(self) -> None:
        index = self._index(extra=("--require-source", "tracked.txt"))
        self.assertEqual(self._verify(index)[0], 0)
        self._git("checkout", "--", "tracked.txt")

        code, report, _ = self._verify(index)

        self.assertEqual(code, 1)
        fields = {difference["field"] for difference in report["differences"]}
        self.assertTrue({"verdict", "missing_required", "failure"} <= fields)

    @staticmethod
    def _with_binary_path(text: str, path: str) -> str:
        document = json.loads(text)
        document["binaries"][0]["path"] = path
        return json.dumps(document)

    def test_malformed_conflicting_or_unsupported_indexes_are_refused(self) -> None:
        index = self._index()
        good = index.read_text(encoding="utf-8")
        cases = {
            "duplicate_key": ('{"verdict":"complete","verdict":"complete"}', "index_invalid"),
            "not_json": ("{", "index_invalid"),
            "not_object": ("[]", "index_invalid"),
            "oversized": (" " * (8 * 1024 * 1024 + 1), "index_too_large"),
            "relative_artifact": (self._with_binary_path(good, "delegate-bin"), "index_invalid"),
            "wrong_type": (good.replace('"unresolved":[]', '"unresolved":"x"'), "index_invalid"),
            "version": (good.replace('"schema_version":1', '"schema_version":true'), "index_version_unsupported"),
            "failed": (good.replace('"verdict":"complete"', '"verdict":"failed"'), "index_declares_failure"),
        }
        for label, (text, code_name) in cases.items():
            with self.subTest(label):
                index.write_text(text, encoding="utf-8")
                code, report, _ = self._verify(index)
                self.assertEqual((code, report["failure"]["code"]), (1, code_name))

        link = self.output_dir / "link.json"
        link.symlink_to(index)
        self.assertEqual(self._verify(link)[1]["failure"]["code"], "index_not_regular")
        self.assertEqual(self._verify(self.output_dir / "absent.json")[1]["failure"]["code"], "index_unavailable")

    def test_mode_selection_keeps_generation_cli_and_refuses_mixed_options(self) -> None:
        index = self._index()
        for arguments in (
            ("--root", os.fspath(self.repo)),
            ("--root", os.fspath(self.repo), "--output", os.fspath(index), "--verify", os.fspath(index)),
        ):
            with self.subTest(arguments=arguments), contextlib.redirect_stderr(io.StringIO()) as stderr:
                self.assertEqual(evidence.main(list(arguments)), 1)
                self.assertEqual(stderr.getvalue(), "handoff evidence: arguments_invalid\n")
        for option in ("--unresolved", "--transition-row", "--require-source", "--baseline"):
            with self.subTest(option):
                self.assertEqual(self._verify(index, (option, "value"))[1]["failure"]["code"], "arguments_invalid")


class AliasedTemporaryVerifyTests(HandoffVerifyTests):
    """The verification contract holds when the temp directory is reached through a symlink."""

    temporary_alias = True

    def test_fixture_is_reached_through_an_alias_but_identified_canonically(self) -> None:
        requested = Path(self.temporary.name)
        self.assertTrue(requested.parent.is_symlink())
        self.assertNotEqual(requested, self.base)
        self.assertEqual(self.base, requested.resolve(strict=True))
        index = self._index()
        entry = self._load(index)["receipts"][0]
        self.assertEqual(entry["path"], os.fspath(self.receipt))


if __name__ == "__main__":
    unittest.main()
