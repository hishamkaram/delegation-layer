"""Tests for the offline Homebrew formula renderer CLI and child closure."""

from __future__ import annotations

import contextlib
from dataclasses import dataclass
import io
import os
from pathlib import Path
import shutil
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.processes.operations import run_bounded_command


SCRIPTS = Path(__file__).resolve().parent
RENDERER = SCRIPTS / "render-homebrew-formula.sh"
TAG = "v1.2.3"
PLATFORMS = ("Darwin_amd64", "Darwin_arm64", "Linux_amd64", "Linux_arm64")
DIGESTS = {platform: f"{index:064x}" for index, platform in enumerate(PLATFORMS, start=10)}
OTHER_ASSET = f"{'e' * 64}  delegation-layer_1.2.3_sbom.json"
REJECTING_TOOL = "#!/bin/sh\necho called >>\"$(dirname \"$0\")/called\"\nexit 99\n"
# Every renderer and Ruby child is finite: one deadline and one bound for both streams.
CHILD_TIMEOUT_SECONDS = 60
CHILD_OUTPUT_LIMIT = 64 * 1024
FIXTURE_PREFIX = "homebrew-formula-test"
FIXTURE_LABEL = "homebrew formula test fixture"


def archive(platform, tag=TAG):
    return f"delegation-layer_{tag[1:]}_{platform}.tar.gz"


def checksum_lines(tag=TAG, digests=DIGESTS):
    return [f"{digests[platform]}  {archive(platform, tag)}" for platform in PLATFORMS]


def expected_formula(repository="hishamkaram/delegation-layer", tag=TAG, digests=DIGESTS):
    def entry(platform):
        url = f"https://github.com/{repository}/releases/download/{tag}/{archive(platform, tag)}"
        return f'url "{url}"', f'sha256 "{digests[platform]}"'

    lines = ["class DelegationLayer < Formula",
             '  desc "Durable supervised delegation for supported AI CLIs"',
             f'  homepage "https://github.com/{repository}"',
             f'  version "{tag[1:]}"', '  license "MIT"', ""]
    for system, (arm, intel) in (("macos", ("Darwin_arm64", "Darwin_amd64")),
                                 ("linux", ("Linux_arm64", "Linux_amd64"))):
        lines += [f"  on_{system} do", "    if Hardware::CPU.arm?",
                  f"      {entry(arm)[0]}", f"      {entry(arm)[1]}", "    else",
                  f"      {entry(intel)[0]}", f"      {entry(intel)[1]}", "    end", "  end", ""]
    lines += ["  def install", '    bin.install "delegate"', '    bin.install "delegate-run"',
              '    libexec.install "pueue", "pueued"', "  end", "", "  test do",
              '    assert_match "delegate #{version}", shell_output("#{bin}/delegate version")',
              '    system bin/"delegate", "providers", "--json"',
              '    assert_match "pueue ", shell_output("#{libexec}/pueue --version")',
              '    assert_match "pueued ", shell_output("#{libexec}/pueued --version")',
              "  end", "end", ""]
    return "\n".join(lines)


@dataclass(frozen=True)
class CommandOutput:
    """One finished child: its exit code and its separately captured streams as text."""

    returncode: int
    stdout: str
    stderr: str


class RendererFixture(unittest.TestCase):
    def setUp(self):
        # The tree removes itself unless a child's closure is unproven; then it is retained as evidence.
        self.tree = FixtureTree.open(prefix=FIXTURE_PREFIX, label=FIXTURE_LABEL)
        self.addCleanup(self.tree.dispose)
        self.root = self.tree.root
        self.fakebin = self.root / "fakebin"
        self.fakebin.mkdir()
        for tool in ("curl", "gh"):
            (self.fakebin / tool).write_text(REJECTING_TOOL)
            (self.fakebin / tool).chmod(0o755)

    def tools_were_called(self):
        return (self.fakebin / "called").exists()

    def write_checksums(self, lines):
        path = self.root / "checksums.txt"
        path.write_text("".join(f"{line}\n" for line in lines))
        return path

    def environment(self, **extra):
        # Only PATH and explicit overrides: no token or inherited configuration reaches the script.
        return {"PATH": f"{self.fakebin}:{os.environ['PATH']}", **extra}

    def run_child(self, argv, environment):
        """Run one bounded child; the tree observes its original failure before the test sees it."""
        result = self.tree.observe_operation(lambda: run_bounded_command(
            argv, environment, max_output=CHILD_OUTPUT_LIMIT, capture=True, capture_stderr=True,
            timeout=CHILD_TIMEOUT_SECONDS))
        return CommandOutput(result.exit_code, result.output.decode(), result.stderr.decode())

    def render(self, tag, checksums, **extra):
        return self.run_child(["bash", str(RENDERER), tag, str(checksums)], self.environment(**extra))


class RenderTests(RendererFixture):
    def test_renders_exact_formula_for_default_repository(self):
        result = self.render(TAG, self.write_checksums(checksum_lines()))
        self.assertEqual((result.returncode, result.stderr), (0, ""))
        self.assertEqual(result.stdout, expected_formula())
        self.assertFalse(self.tools_were_called())

    def test_repository_override_changes_homepage_and_every_url(self):
        result = self.render(TAG, self.write_checksums(checksum_lines()),
                             DELEGATION_LAYER_REPOSITORY="fork-owner/delegation_layer.fork")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, expected_formula("fork-owner/delegation_layer.fork"))
        self.assertNotIn("hishamkaram", result.stdout)

    def test_empty_repository_override_uses_the_default(self):
        result = self.render(TAG, self.write_checksums(checksum_lines()), DELEGATION_LAYER_REPOSITORY="")
        self.assertEqual(result.stdout, expected_formula())

    def test_tag_selects_the_version_and_archive_names(self):
        digests = {platform: f"{index:064x}" for index, platform in enumerate(PLATFORMS, start=90)}
        tag = "v10.20.30"
        result = self.render(tag, self.write_checksums(checksum_lines(tag, digests)))
        self.assertEqual(result.stdout, expected_formula(tag=tag, digests=digests))
        for platform in PLATFORMS:
            self.assertEqual(result.stdout.count(archive(platform, tag)), 1)

    def test_archives_may_share_a_digest_when_each_has_one_entry(self):
        shared = DIGESTS["Darwin_amd64"]
        digests = {**DIGESTS, "Darwin_arm64": shared, "Linux_arm64": shared}
        result = self.render(TAG, self.write_checksums(checksum_lines(digests=digests)))
        self.assertEqual((result.returncode, result.stderr), (0, ""))
        self.assertEqual(result.stdout, expected_formula(digests=digests))

    def test_output_is_deterministic_and_ignores_line_order_and_other_assets(self):
        shuffled = [OTHER_ASSET, *reversed(checksum_lines()), f"{'d' * 64}  checksums.txt.sig"]
        first = self.render(TAG, self.write_checksums(checksum_lines()))
        second = self.render(TAG, self.write_checksums(shuffled))
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(first.stdout, second.stdout)

    def test_uppercase_digest_is_emitted_lowercase(self):
        digests = {**DIGESTS, "Darwin_amd64": "abcdef" * 10 + "abcd"}
        lines = checksum_lines(digests=digests)
        lines[0] = lines[0].upper().replace(archive("Darwin_amd64").upper(), archive("Darwin_amd64"))
        result = self.render(TAG, self.write_checksums(lines))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, expected_formula(digests=digests))

    def test_formula_preserves_install_layout_and_ruby_syntax(self):
        result = self.render(TAG, self.write_checksums(checksum_lines()))
        for line in ('bin.install "delegate"', 'bin.install "delegate-run"',
                     'libexec.install "pueue", "pueued"', 'license "MIT"'):
            self.assertIn(line, result.stdout)
        self.assertNotIn('depends_on "pueue"', result.stdout)
        formula = self.root / "delegation-layer.rb"
        formula.write_text(result.stdout)
        if shutil.which("ruby") is None:
            self.skipTest("ruby is not installed")
        ruby = self.run_child(["ruby", "-c", str(formula)], dict(os.environ))
        self.assertEqual((ruby.returncode, ruby.stdout.strip()), (0, "Syntax OK"), ruby.stderr)


class RefusalTests(RendererFixture):
    def assert_refused(self, tag, lines, message, **extra):
        result = self.render(tag, self.write_checksums(lines), **extra)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertIn(message, result.stderr)
        self.assertFalse(self.tools_were_called())

    def test_invalid_tags_are_refused_without_output(self):
        for tag in ("1.2.3", "v1.2", "v1.2.3-rc.1", "v1.2.3+meta", "vx.y.z", "v1.2.3\n", "v1.2.3 ", ""):
            with self.subTest(tag=tag):
                self.assert_refused(tag, checksum_lines(), "stable semantic version tag required")

    def test_invalid_repositories_are_refused_without_output(self):
        for repository in ("owner", "owner/", "/repo", "a/b/c", "owner/re po", 'owner/re"po', "owner/re$po",
                           "owner/`id`", "owner/..", "owner/.hidden", "-owner/repo", "owner/repo\n"):
            with self.subTest(repository=repository):
                self.assert_refused(TAG, checksum_lines(), "owner/repository required",
                                    DELEGATION_LAYER_REPOSITORY=repository)

    def test_each_missing_expected_entry_is_refused(self):
        for platform in PLATFORMS:
            with self.subTest(platform=platform):
                lines = [line for line in checksum_lines() if archive(platform) not in line]
                self.assert_refused(TAG, lines, f"missing checksum for {archive(platform)}")

    def test_entries_for_another_version_do_not_satisfy_the_tag(self):
        self.assert_refused(TAG, checksum_lines("v1.2.4"), "missing checksum for")

    def test_duplicate_expected_entries_are_refused_even_when_identical(self):
        lines = checksum_lines()
        for duplicate in (lines[1], f"{'9' * 64}  {archive('Linux_arm64')}"):
            with self.subTest(duplicate=duplicate):
                self.assert_refused(TAG, [*lines, duplicate], "duplicate checksum entries for")

    def test_malformed_digests_are_refused(self):
        archive_name = archive("Linux_amd64")
        others = [line for line in checksum_lines() if archive_name not in line]
        for entry in (f"{'a' * 63}  {archive_name}", f"{'a' * 65}  {archive_name}", f"{'g' * 64}  {archive_name}",
                      f"{'a' * 64} extra {archive_name}", f"{'a' * 64}  {archive_name} trailing"):
            with self.subTest(entry=entry):
                result = self.render(TAG, self.write_checksums([*others, entry]))
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertRegex(result.stderr, r"malformed checksum for|missing checksum for")

    def test_path_prefixed_names_are_not_expected_entries(self):
        lines = [f"{DIGESTS[platform]}  ./{archive(platform)}" for platform in PLATFORMS]
        self.assert_refused(TAG, lines, "missing checksum for")

    def test_unreadable_checksum_paths_are_refused(self):
        for path in (self.root / "absent.txt", self.root, Path("-")):
            with self.subTest(path=str(path)):
                result = self.run_child(["bash", str(RENDERER), TAG, str(path)], self.environment())
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")
                self.assertIn("readable checksums file required", result.stderr)

    def test_checksums_path_with_spaces_is_read(self):
        directory = self.root / "with space"
        directory.mkdir()
        path = directory / "checksums.txt"
        path.write_text("".join(f"{line}\n" for line in checksum_lines()))
        result = self.render(TAG, path)
        self.assertEqual((result.returncode, result.stdout), (0, expected_formula()))

    def test_wrong_argument_count_prints_usage(self):
        result = self.run_child(["bash", str(RENDERER), TAG], self.environment())
        self.assertEqual((result.returncode, result.stdout), (2, ""))
        self.assertIn("usage: render-homebrew-formula.sh", result.stderr)


class ChildClosureControlTests(RendererFixture):
    """The tree, not the test, decides disposition from each child's original closure fact."""

    def dispose_reporting(self):
        report = io.StringIO()
        with contextlib.redirect_stderr(report):
            removed = self.tree.dispose()
        return removed, report.getvalue()

    def test_streams_share_the_named_bound_and_a_closed_overflow_removes_the_tree(self):
        half = CHILD_OUTPUT_LIMIT // 2
        emit = ("import sys; sys.stdout.buffer.write(b'o' * {out}); sys.stdout.flush(); "
                "sys.stderr.buffer.write(b'e' * {err})")
        exact = self.run_child([sys.executable, "-c", emit.format(out=half, err=CHILD_OUTPUT_LIMIT - half)],
                               self.environment())
        self.assertEqual(exact, CommandOutput(0, "o" * half, "e" * (CHILD_OUTPUT_LIMIT - half)))
        (self.root / "evidence").write_text("child output")
        with self.assertRaises(BoundedCommandError) as caught:
            self.run_child([sys.executable, "-c", emit.format(out=half, err=CHILD_OUTPUT_LIMIT - half + 1)],
                           self.environment())
        self.assertIs(caught.exception.reason, CommandFailure.OUTPUT_EXCEEDED)
        self.assertIsNone(self.tree.unresolved)
        self.assertEqual(self.dispose_reporting(), (True, ""))
        self.assertFalse(self.root.exists())

    def test_unknown_closure_retains_the_tree_until_no_launch_is_proven(self):
        (self.root / "evidence").write_text("child output")
        # A synthetic unknown closure: the only spawn attempt raises before any child exists.
        unknown = operations.subprocess.TimeoutExpired("synthetic-spawn", CHILD_TIMEOUT_SECONDS)
        with patch.object(operations.subprocess, "Popen", side_effect=unknown) as spawn, \
                self.assertRaises(BoundedCommandError) as caught:
            self.render(TAG, self.write_checksums(checksum_lines()))
        self.assertIs(caught.exception.reason, CommandFailure.TIMEOUT)
        self.assertIs(self.tree.unresolved, caught.exception)
        removed, report = self.dispose_reporting()
        self.assertFalse(removed)
        self.assertEqual(report, f"{FIXTURE_LABEL} retained (command_timeout): {self.root}\n")
        self.assertEqual((self.root / "evidence").read_text(), "child output")
        # Manual settlement needs proof that nothing can still write: no child was ever launched.
        self.assertEqual(spawn.call_count, 1)
        self.assertFalse(self.tools_were_called())
        self.tree.remove_retained()
        self.assertFalse(self.root.exists())


if __name__ == "__main__":
    unittest.main()
