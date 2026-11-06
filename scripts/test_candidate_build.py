"""End-to-end tests for the development-candidate builder CLI over a real Git fixture.

The fixture repository carries a copy of the harness; go, goreleaser, npm and
node are fake executables. No test reaches the network, a token or a provider.
Real GoReleaser, `go version -m` on built binaries and Mac execution are
proven separately.
"""

import contextlib
import io
import json
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import test_candidate_build_support as support
from test_candidate_build_support import FixtureCase
from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure,
)
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.release_verification import candidate_build
from scripts.delegate_acceptance.release_verification.candidate_build_artifacts import BuildContext
from scripts.delegate_acceptance.release_verification.candidate_build_plan import (
    BuildRequest, build_inputs, build_manifest,
)
from scripts.delegate_acceptance.release_verification.candidate_build_source import child_environment
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    MANIFEST_PATH, ArtifactKind, encode_manifest, expected_inventory, parse_manifest,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError


PAYLOAD_NAMES = sorted([path for path, _kind, _os, _arch in expected_inventory(support.VERSION)]
                        + ["candidate-manifest.json"])
COMMAND_NAMES = [
    "git-toplevel", "git-head", "git-tree", "git-status", "git-base-commit", "git-base-ancestor",
    "git-clone", "git-checkout", "git-head", "git-tree", "git-status", "git-origin", "git-tag",
    "go-version", "go-env-goroot", "goreleaser-version", "python-version", "node-version",
    "fetch-pueue", "goreleaser-release", "npm-pack", "render-formula", *(["go-version-m"] * 8),
]
DECOY_VARIABLES = {"GITHUB_TOKEN": "decoy", "GH_TOKEN": "decoy", "NODE_AUTH_TOKEN": "decoy",
                   "HOMEBREW_TAP_TOKEN": "decoy", "NPM_TOKEN": "decoy", "GOFLAGS": "-buildvcs=false",
                   "DELEGATION_LAYER_REPOSITORY": "attacker/fork"}


class SuccessTests(FixtureCase):
    def test_builds_the_exact_payload_manifest_and_receipt(self):
        before = self.source_snapshot()
        result = self.build(environment=self.fixture.environment(**DECOY_VARIABLES))
        self.assertEqual((result.exit_code, result.output), (0, ""))
        payload = self.scratch / "payload"
        self.assertEqual(sorted(path.name for path in payload.iterdir()), PAYLOAD_NAMES)
        manifest = parse_manifest((payload / "candidate-manifest.json").read_bytes())
        self.assertEqual((manifest.version, manifest.build_tag), ("0.1.37", "v0.1.37"))
        self.assertEqual((manifest.source.commit, manifest.source.tree, manifest.source.base_commit),
                         (self.fixture.head, self.fixture.tree, self.fixture.base))
        toolchains = manifest.build.toolchains
        self.assertEqual((toolchains.go, toolchains.goreleaser, toolchains.node), ("1.27.3", "2.18.1", "22.11.0"))
        self.assertRegex(toolchains.python, r"^\d+\.\d+\.\d+$")
        for entry in manifest.artifacts:
            data = (payload / entry.path).read_bytes()
            self.assertEqual((entry.size, entry.sha256), (len(data), sha(data)), entry.path)
        self.assertEqual(manifest.build.go_sum_sha256,
                         sha((self.fixture.repository / "go.sum").read_bytes()))
        self.assertEqual(manifest.build.goreleaser_config_sha256,
                         sha((self.fixture.repository / ".goreleaser.yaml").read_bytes()))
        self.assertFalse((self.scratch / "work").exists())
        self.assertEqual(self.source_snapshot(), before)
        self.assertEqual(before[1], "")

    def test_receipt_records_commands_without_remote_or_publication_actions(self):
        self.assertEqual(self.build().exit_code, 0)
        receipt = self.receipt()
        self.assertEqual((receipt["verdict"], receipt["stage"], receipt["failure"]), ("pass", "complete", None))
        self.assertEqual([record["name"] for record in receipt["commands"]], COMMAND_NAMES)
        self.assertTrue(all(record["exit_code"] == 0 for record in receipt["commands"]))
        argv = {record["name"]: record["argv"] for record in receipt["commands"]}
        self.assertEqual(argv["goreleaser-release"][1:], ["release", "--clean", "--skip=publish"])
        self.assertEqual(argv["fetch-pueue"], ["bash", "scripts/fetch-pueue-assets.sh", ".release/pueue"])
        self.assertEqual(argv["git-tag"][:2], ["git", "tag"])
        self.assertEqual(argv["git-tag"][2:], ["v0.1.37", self.fixture.head])
        flat = [word for record in receipt["commands"] for word in record["argv"]]
        for forbidden in ("push", "--snapshot", "--skip=validate", "gh", "attestation", "-buildvcs=false"):
            self.assertNotIn(forbidden, flat)
        self.assertEqual(receipt["manifest_sha256"],
                         sha((self.scratch / "payload" / "candidate-manifest.json").read_bytes()))

    def test_build_children_never_see_credentials_or_build_flag_overrides(self):
        result = self.build(environment=self.fixture.environment(**DECOY_VARIABLES))
        self.assertEqual(result.exit_code, 0, result.output)
        names = self.fixture.tool_environment_names()
        self.assertTrue(names)
        self.assertFalse(names & set(DECOY_VARIABLES), names & set(DECOY_VARIABLES))
        self.assertIn("GOTOOLCHAIN", names)
        self.assertNotIn("decoy", (self.scratch / "receipt.json").read_text())

    def test_formula_is_the_canonical_renderer_output_of_the_built_checksums(self):
        self.assertEqual(self.build().exit_code, 0)
        payload = self.scratch / "payload"
        rendered = self.fixture.render(self.case, "v0.1.37", payload / "checksums.txt")
        self.assertEqual((payload / "delegation-layer.rb").read_bytes(), rendered)
        manifest = parse_manifest((payload / "candidate-manifest.json").read_bytes())
        for entry in manifest.artifacts:
            if entry.kind is ArtifactKind.ARCHIVE:
                self.assertIn(f'url "https://github.com/hishamkaram/delegation-layer/releases/download/'
                              f'v0.1.37/{entry.path}"', rendered.decode())
                self.assertIn(f'sha256 "{entry.sha256}"', rendered.decode())

    def test_identical_inputs_build_identical_candidates(self):
        self.assertEqual(self.build().exit_code, 0)
        second = self.scratch / "second"
        second.mkdir()
        again = self.run_builder(*self.fixture.build_arguments(second))
        self.assertEqual(again.exit_code, 0, again.output)
        for name in PAYLOAD_NAMES:
            self.assertEqual((self.scratch / "payload" / name).read_bytes(),
                             (second / "payload" / name).read_bytes(), name)


class RefusalTests(FixtureCase):
    def assert_refused(self, result, code, *, stage=None):
        self.assertEqual(result.exit_code, 1, result.output)
        self.assertIn(code, result.output)
        if stage:
            self.assertIn(stage, result.output)
        self.assertFalse((self.scratch / "payload").exists())
        self.assertFalse((self.scratch / "work").exists())
        receipt = self.receipt()
        self.assertEqual((receipt["verdict"], receipt["failure"]["code"]), ("fail", code))

    def test_dirty_or_untracked_source_is_refused(self):
        repository = self.fixture.repository
        for change in (lambda: (repository / "go.sum").write_text("changed\n"),
                       lambda: (repository / "stray.txt").write_text("x\n")):
            with self.subTest(change=change):
                self.fixture.require_resolved("fixture source change")
                change()
                self.assert_refused(self.build(), "source_not_clean")
                self.fixture.restore()
                (self.scratch / "receipt.json").unlink()

    def test_head_base_version_and_root_must_match_the_request(self):
        self.assert_refused(self.build(commit=self.fixture.base), "source_head_mismatch")
        (self.scratch / "receipt.json").unlink()
        self.assert_refused(self.build(**{"base-commit": "1" * 40}), "command_failed", stage="git-base-commit")
        (self.scratch / "receipt.json").unlink()
        self.assert_refused(self.build(version="0.1.38"), "package_version_mismatch")

    def test_base_that_is_not_an_ancestor_is_refused(self):
        unrelated = support.git(self.fixture.repository, "commit-tree", self.fixture.tree, "-m", "unrelated",
                                tree=self.fixture.fixture_tree)
        self.assert_refused(self.build(**{"base-commit": unrelated}), "command_failed",
                            stage="git-base-ancestor")

    def test_malformed_identity_and_foreign_root_are_refused_before_any_work(self):
        cases: tuple[dict[str, str], ...] = (
            {"version": "0.1.37-rc.1"}, {"commit": "abc"}, {"repository": "no-slash"})
        for overrides in cases:
            with self.subTest(overrides=overrides):
                result = self.run_builder(*self.fixture.build_arguments(self.scratch, **overrides))
                self.assertEqual(result.exit_code, 1)
                self.assertIn("candidate_field_invalid", result.output)
                self.assertFalse((self.scratch / "work").exists())
        self.fixture.require_resolved("foreign source root creation")
        foreign = self.scratch / "elsewhere"
        foreign.mkdir()
        result = self.build(**{"source-root": str(foreign)})
        self.assertEqual(result.exit_code, 1)
        self.assertIn("source_root_not_harness", result.output)

    def test_paths_inside_the_source_or_already_present_are_refused_without_harm(self):
        inside = self.build(**{"output-dir": str(self.fixture.repository / "payload")})
        self.assertIn("build_path_inside_source", inside.output)
        self.assertFalse((self.fixture.repository / "payload").exists())
        existing = self.scratch / "payload"
        existing.mkdir()
        (existing / "keep.txt").write_text("keep")
        self.assert_refused_existing(self.build())
        self.assertEqual((existing / "keep.txt").read_text(), "keep")

    def assert_refused_existing(self, result):
        self.assertEqual(result.exit_code, 1)
        self.assertIn("build_directory_unavailable", result.output)
        self.assertFalse((self.scratch / "work").exists())

    def test_receipt_aliasing_either_owned_tree_is_refused_before_any_effect(self):
        link = self.scratch / "link.json"
        link.symlink_to(self.scratch / "payload" / "candidate-manifest.json")
        aliases = {
            "manifest": self.scratch / "payload" / "candidate-manifest.json",
            "checksums": self.scratch / "payload" / "checksums.txt",
            "payload directory": self.scratch / "payload",
            "scratch file": self.scratch / "work" / "receipt.json",
            "scratch clone": self.scratch / "work" / "clone" / "receipt.json",
            "scratch directory": self.scratch / "work",
            "symlink to the manifest": link,
        }
        for name, receipt in aliases.items():
            with self.subTest(alias=name):
                result = self.build(receipt=str(receipt))
                self.assertEqual(result.exit_code, 1, result.output)
                self.assertIn("build_receipt_inside_owned_tree", result.output)
                self.assertEqual([path.name for path in self.scratch.iterdir()], ["link.json"])
                self.assertEqual(self.fixture.tool_environment_names(), set())

    def test_receipt_outside_both_trees_in_a_new_directory_is_accepted(self):
        result = self.build(receipt=str(self.scratch / "evidence" / "build.json"))
        self.assertEqual((result.exit_code, result.output), (0, ""))
        self.assertEqual(json.loads((self.scratch / "evidence" / "build.json").read_text())["verdict"], "pass")

    def test_wrong_go_toolchain_or_goroot_is_refused(self):
        self.fixture.set_mode("go", "old")
        self.assert_refused(self.build(), "go_toolchain_pin_mismatch")
        (self.scratch / "receipt.json").unlink()
        self.fixture.set_mode("go", "foreign-root")
        self.assert_refused(self.build(), "go_root_path_mismatch")

    def test_each_corrupt_or_missing_build_output_is_refused(self):
        cases = {
            ("goreleaser", "fail"): ("command_failed", "goreleaser-release"),
            ("goreleaser", "drop-archive"): ("goreleaser_output_missing", None),
            ("goreleaser", "extra-checksum"): ("checksum_entries_not_exact", None),
            ("goreleaser", "corrupt-checksum"): ("archive_checksum_mismatch", None),
            ("goreleaser", "wrong-pueue"): ("bundled_pueue_mismatch", None),
            ("goreleaser", "wrong-revision"): ("go_vcs_revision_mismatch", None),
            ("goreleaser", "modified"): ("go_vcs_modified", None),
            ("npm", "wrong-name"): ("npm_pack_output_invalid", None),
            ("npm", "stray-file"): ("npm_pack_destination_unexpected", None),
        }
        for (tool, mode), (code, stage) in cases.items():
            with self.subTest(tool=tool, mode=mode):
                self.fixture.clear_modes()
                self.fixture.set_mode(tool, mode)
                self.assert_refused(self.build(), code, stage=stage)
                (self.scratch / "receipt.json").unlink()

    def test_missing_required_argument_is_a_usage_error(self):
        result = self.run_builder("build", "--version", support.VERSION)
        self.assertEqual(result.exit_code, 2)


class FixtureIdentityTests(FixtureCase):
    def test_fixture_exposes_the_git_tree_and_a_native_tool_path(self):
        self.assertRegex(self.fixture.tree, r"^[0-9a-f]{40}$")
        self.assertEqual(self.fixture.environment()["PATH"].split(os.pathsep)[0], str(self.fixture.tools / "bin"))


class BuilderBoundaryTests(unittest.TestCase):
    """CandidateFixture.builder over dedicated trees: nothing here touches the shared class fixture.

    The bounded command owner is replaced by finite fake results, and the canonical
    Popen entry point refuses and records every launch. Only after zero launches are
    proven are the synthetic retained trees removed, by the shared identity-pinned path.
    """

    def setUp(self):
        self.baseline = open_descriptors()
        self.launches: list[tuple] = []
        self.launch_free = False
        self.checking_retention = False
        self.addCleanup(self.remove_synthetic_trees)
        self.outer = FixtureTree.open(prefix="candidate-boundary-fixture", label="candidate boundary fixture")
        self.addCleanup(self.outer.dispose)
        self.fixture = support.CandidateFixture(self.outer)
        self.case = FixtureTree.open(self.outer.root, prefix="candidate-boundary-case",
                                     label="candidate boundary case")
        self.addCleanup(self.case.dispose)
        self.arguments = self.fixture.build_arguments(self.case.root)
        self.receipt_path = self.case.root / "receipt.json"

    def remove_synthetic_trees(self):
        """Remove retained synthetic trees, but only after zero launches were proven."""
        if not self.launch_free or self.checking_retention:
            return
        for tree in (self.case, self.outer):
            if tree.root.exists() and not tree.dispose():
                tree.remove_retained()

    def refuse_launch(self, *arguments, **keywords):
        self.launches.append(arguments)
        raise AssertionError("a real child must not launch in a boundary test")

    def run_builder(self, *, fake):
        """Run the real builder wrapper with a fake command owner; return what it raised, if anything."""
        failure = None
        with patch.object(support, "run_bounded_command", side_effect=fake) as owner, \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch):
            try:
                try:
                    self.fixture.builder(self.case, *self.arguments)
                except VerificationError as error:
                    failure = error
            finally:
                # Recorded on every exit while the oracle is installed, so a failure inside the
                # builder after the owners were marked unresolved still allows synthetic cleanup.
                self.launch_free = not self.launches
        owner.assert_called_once()
        self.assertEqual(self.launches, [])
        return failure

    def fake_receipt(self, receipt, *, exit_code=1):
        def fake(argv, environment, **keywords):
            self.receipt_path.write_text(json.dumps(receipt))
            return BoundedCommandResult(exit_code, b"candidate build failed: fixture\n")
        return fake

    def settle(self, *, retained):
        """Run the registered case and containing-owner cleanups, then check disposition and pins."""
        stderr = io.StringIO()
        self.checking_retention = True
        try:
            with contextlib.redirect_stderr(stderr):
                self.assertTrue(self.doCleanups())
            self.assertEqual(open_descriptors(), self.baseline)
            for tree in (self.case, self.outer):
                self.assertEqual(tree.root.is_dir(), retained)
                self.assertEqual(tree.dispose(), not retained)
                self.assertEqual(tree.unresolved is not None, retained)
                self.assertEqual(f"{tree.label} retained" in stderr.getvalue(), retained)
        finally:
            self.checking_retention = False
            self.remove_synthetic_trees()
        self.assertFalse(self.case.root.exists() or self.outer.root.exists())
        self.assertEqual(self.case.unresolved is not None, retained)

    def test_unresolved_failure_from_the_command_owner_retains_both_owners(self):
        failure = self.run_builder(fake=BoundedCommandError(CommandFailure.TIMEOUT))
        self.assertEqual((failure.code, failure.stage), ("command_timeout", "fixture-cli"))
        self.settle(retained=True)
        assert isinstance(self.case.unresolved, VerificationError)
        assert isinstance(self.outer.unresolved, VerificationError)
        self.assertEqual((self.case.unresolved.code, self.outer.unresolved.code),
                         ("command_timeout", "command_timeout"))

    def test_terminal_receipt_with_an_unresolved_code_retains_both_owners(self):
        receipt = {"verdict": "fail", "failure": {"code": "command_termination_failed", "stage": "x"},
                   "cleanup": {"dispositions": {"scratch": "retained", "payload": "retained"}}}
        self.assertIsNone(self.run_builder(fake=self.fake_receipt(receipt)))
        self.settle(retained=True)
        assert isinstance(self.outer.unresolved, VerificationError)
        self.assertEqual(self.outer.unresolved.code, "command_termination_failed")

    def test_failed_build_that_retained_a_tree_retains_both_owners(self):
        receipt = {"verdict": "fail", "failure": {"code": "build_unexpected_error", "stage": "x"},
                   "cleanup": {"dispositions": {"scratch": "retained", "payload": "retained"}}}
        self.assertIsNone(self.run_builder(fake=self.fake_receipt(receipt)))
        self.settle(retained=True)
        unresolved = self.case.unresolved
        if not isinstance(unresolved, VerificationError):
            self.fail(f"nested builder retained a {type(unresolved).__name__}, not a VerificationError")
        self.assertEqual(unresolved.stage, "nested-builder")

    def test_closed_failure_does_not_retain_either_owner(self):
        receipt = {"verdict": "fail", "failure": {"code": "command_failed", "stage": "x"},
                   "cleanup": {"dispositions": {"scratch": "removed", "payload": "removed"}}}
        self.assertIsNone(self.run_builder(fake=self.fake_receipt(receipt)))
        self.settle(retained=False)

    def test_successful_build_with_a_retained_payload_does_not_retain_either_owner(self):
        receipt = {"verdict": "pass", "failure": None,
                   "cleanup": {"dispositions": {"scratch": "removed", "payload": "retained"}}}
        self.assertIsNone(self.run_builder(fake=self.fake_receipt(receipt, exit_code=0)))
        self.settle(retained=False)


class FixtureGitClosureTests(unittest.TestCase):
    """support.git reports an unproven child closure to the owning FixtureTree before the error escapes.

    The canonical command owner is replaced by a finite fake that raises one error object, and the
    canonical Popen entry point refuses and records launches; synthetic retained trees are removed
    only after zero launches were proven.
    """

    UNRESOLVED = "command_termination_failed"

    def setUp(self):
        self.baseline = open_descriptors()
        self.launches: list[tuple] = []
        self.zero_launches = False
        self.checking_retention = False
        self.addCleanup(self.remove_synthetic_tree)
        self.tree = FixtureTree.open(prefix="candidate-git-closure", label="candidate git closure")
        self.addCleanup(self.tree.dispose)

    def remove_synthetic_tree(self):
        if self.zero_launches and not self.checking_retention and self.tree.root.exists() \
                and not self.tree.dispose():
            self.tree.remove_retained()

    def refuse_launch(self, *arguments, **keywords):
        self.launches.append(arguments)
        raise AssertionError("a real child must not launch in a closure test")

    def attempt(self, action, failure):
        """Run ``action`` against a fake command owner that raises ``failure``; return what escaped."""
        with patch.object(support, "command", side_effect=failure) as owner, \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch):
            try:
                with self.assertRaises(VerificationError) as caught:
                    action()
            finally:
                self.zero_launches = not self.launches
        owner.assert_called_once()
        self.assertEqual(self.launches, [])
        self.assertIs(caught.exception, failure)

    def settle(self, *, retained):
        """Run the registered cleanups, then check the disposition and that every pin was consumed."""
        self.checking_retention = True
        try:
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertTrue(self.doCleanups())
            self.assertEqual(open_descriptors(), self.baseline)
            self.assertEqual(self.tree.root.is_dir(), retained)
            self.assertEqual(self.tree.dispose(), not retained)
        finally:
            self.checking_retention = False
            self.remove_synthetic_tree()
        self.assertFalse(self.tree.root.exists())

    def direct_action(self):
        repository = self.tree.root / "repository"
        repository.mkdir()
        return lambda: support.git(repository, "status", tree=self.tree)

    def setup_action(self):
        return lambda: support.CandidateFixture(self.tree)

    def check_unresolved(self, action):
        failure = VerificationError(self.UNRESOLVED, "fixture-git")
        self.attempt(action, failure)
        self.assertIs(self.tree.unresolved, failure)
        self.settle(retained=True)
        self.assertIs(self.tree.unresolved, failure)

    def check_closed(self, action):
        failure = VerificationError("command_failed", "fixture-git")
        self.attempt(action, failure)
        self.assertIsNone(self.tree.unresolved)
        self.settle(retained=False)

    def test_unresolved_closure_of_a_direct_git_call_retains_the_owning_tree(self):
        self.check_unresolved(self.direct_action())

    def test_unresolved_closure_during_fixture_setup_retains_the_owning_tree(self):
        self.check_unresolved(self.setup_action())

    def test_unresolved_closure_during_restore_retains_the_owning_tree(self):
        fixture = support.CandidateFixture(self.tree)
        self.check_unresolved(fixture.restore)

    def test_unresolved_closure_of_the_renderer_retains_the_case_and_its_fixture(self):
        fixture = support.CandidateFixture(self.tree)
        case = FixtureTree.open(self.tree.root, prefix="candidate-render-case", label="candidate render case")
        self.addCleanup(case.dispose)
        failure = VerificationError(self.UNRESOLVED, "fixture-render")
        self.attempt(lambda: fixture.render(case, "v0.1.37", case.root / "checksums.txt"), failure)
        self.assertIs(case.unresolved, failure)
        self.assertIs(self.tree.unresolved, failure)
        self.settle(retained=True)

    def test_restoration_beneath_an_unresolved_child_is_refused_without_any_command(self):
        fixture = support.CandidateFixture(self.tree)
        fixture.set_mode("go", "old")
        failure = VerificationError(self.UNRESOLVED, "fixture-git")
        self.attempt(lambda: support.git(fixture.repository, "status", tree=self.tree), failure)
        with patch.object(support, "command") as owner, \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch):
            try:
                for restoration in (fixture.restore, fixture.clear_modes):
                    with self.subTest(restoration=restoration.__name__):
                        with self.assertRaisesRegex(RuntimeError, "no proven closure"):
                            restoration()
            finally:
                self.zero_launches = not self.launches
        owner.assert_not_called()
        self.assertEqual((fixture.tools / "modes" / "go").read_text(), "old")
        self.assertIs(self.tree.unresolved, failure)
        self.settle(retained=True)

    def reuse_fixture(self):
        fixture = support.CandidateFixture(self.tree)
        fixture.set_mode("go", "old")
        case = FixtureTree.open(self.tree.root, prefix="candidate-reuse-case", label="candidate reuse case")
        self.addCleanup(case.dispose)
        return fixture, case

    def reuse(self, fixture, case, receipt, *, refused):
        """Set a mode, launch the builder and run an in-process build against finite fake owners."""
        callbacks = []
        actions = {"set_mode": lambda: fixture.set_mode("go", "foreign-root"),
                   "builder": lambda: fixture.builder(case, "build", "--receipt", str(receipt)),
                   "run_in_process": lambda: fixture.run_in_process(case, lambda: callbacks.append("build"), receipt)}
        with patch.object(support, "run_bounded_command", return_value=BoundedCommandResult(1, b"")) as owner, \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch):
            try:
                for name, action in actions.items():
                    with self.subTest(action=name):
                        if refused:
                            with self.assertRaisesRegex(RuntimeError, "no proven closure"):
                                action()
                        else:
                            action()
            finally:
                self.zero_launches = not self.launches
        return owner.call_count, callbacks

    def test_mutation_and_reuse_beneath_an_unresolved_child_are_refused_without_any_effect(self):
        fixture, case = self.reuse_fixture()
        stale = case.root / "receipt.json"
        stale.write_text(json.dumps({"verdict": "fail", "failure": {"code": self.UNRESOLVED, "stage": "x"},
                                     "cleanup": {"dispositions": {"scratch": "retained"}}}))
        failure = VerificationError(self.UNRESOLVED, "fixture-git")
        self.attempt(lambda: support.git(fixture.repository, "status", tree=self.tree), failure)
        self.assertEqual(self.reuse(fixture, case, stale, refused=True), (0, []))
        self.assertEqual((fixture.tools / "modes" / "go").read_text(), "old")
        self.assertIsNone(case.unresolved)
        self.assertIs(self.tree.unresolved, failure)
        self.settle(retained=True)

    def test_mutation_and_reuse_of_a_resolved_fixture_proceed(self):
        fixture, case = self.reuse_fixture()
        self.assertEqual(self.reuse(fixture, case, case.root / "absent.json", refused=False), (1, ["build"]))
        self.assertEqual((fixture.tools / "modes" / "go").read_text(), "foreign-root")
        self.assertEqual((case.unresolved, self.tree.unresolved), (None, None))
        self.settle(retained=False)

    def test_closed_failure_of_a_direct_git_call_leaves_the_tree_removable(self):
        self.check_closed(self.direct_action())

    def test_closed_failure_during_fixture_setup_leaves_the_tree_removable(self):
        self.check_closed(self.setup_action())


class FixtureGitMaintenanceTests(unittest.TestCase):
    def test_fixture_git_starts_no_maintenance_child_even_when_forced_to_the_foreground(self):
        tree = FixtureTree.open(prefix="candidate-git-probe", label="candidate git probe")
        self.addCleanup(tree.dispose)
        repository, trace = tree.root / "repository", tree.root / "trace2.jsonl"
        repository.mkdir()
        with patch.dict(support.GIT_ENVIRONMENT, {"GIT_TRACE2_EVENT": str(trace)}):
            support.git(repository, "init", "--quiet", tree=tree)
            # Foreground is forced in this probe only, so a maintenance child would finish before cleanup.
            commit = ["-c", "maintenance.autoDetach=false", "commit", "--allow-empty", "--quiet", "-m", "probe"]
            support.git(repository, *commit, tree=tree)
        events = [json.loads(line) for line in trace.read_text().splitlines()]
        commits = [event["sid"] for event in events
                   if event["event"] == "start" and "commit" in event["argv"]
                   and "maintenance.autoDetach=false" in event["argv"]]
        self.assertEqual(len(commits), 1)
        self.assertIn({"event": "exit", "code": 0}, [{"event": event["event"], "code": event.get("code")}
                                                      for event in events if event["sid"] == commits[0]])
        children = [event["argv"] for event in events if event["event"] == "child_start"]
        self.assertFalse([argv for argv in children if "maintenance" in argv], children)


class BuilderGuardRegressionTests(unittest.TestCase):
    """An unexpected failure inside the builder after both owners were marked unresolved."""

    def test_synthetic_trees_are_removed_when_the_builder_fails_after_observing_closure(self):
        reached = {"guarded call": False, "injection": False}
        original = support.CandidateFixture._observe

        def observe_then_fail(fixture, error, case):
            original(fixture, error, case)
            reached["injection"] = True
            raise AssertionError("injected failure after observation")

        def command_owner(argv, environment, **keywords):
            reached["guarded call"] = True
            raise BoundedCommandError(CommandFailure.TIMEOUT)

        class Inner(BuilderBoundaryTests):
            def test_failure_after_observation(inner):
                with patch.object(support.CandidateFixture, "_observe", observe_then_fail):
                    inner.run_builder(fake=command_owner)

        inner = Inner("test_failure_after_observation")
        self.addCleanup(self.remove_leftovers, inner, reached)
        before = open_descriptors()
        result = unittest.TestResult()
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            inner.run(result)
        self.assertEqual((result.testsRun, result.errors), (1, []))
        ((_, trace),) = result.failures
        self.assertIn("injected failure after observation", trace)
        self.assertEqual(reached, {"guarded call": True, "injection": True})
        self.assertEqual(inner.launches, [])
        self.assertTrue(inner.launch_free)
        self.assertEqual(open_descriptors(), before)
        for tree in (inner.case, inner.outer):
            self.assertFalse(tree.root.exists())
            assert isinstance(tree.unresolved, VerificationError)
            self.assertEqual(tree.unresolved.code, "command_timeout")
            self.assertIn(f"{tree.label} retained (command_timeout)", stderr.getvalue())

    def remove_leftovers(self, inner, reached):
        """Red-run hygiene: remove the inner synthetic trees only after independent positive proof."""
        proven = (all(reached.values()) and inner.launches == [] and hasattr(inner, "arguments"))
        if not proven:
            return
        for tree in (inner.case, inner.outer):
            if tree.root.exists() and not tree.dispose():
                tree.remove_retained()


class RetryLoopReuseRegressionTests(unittest.TestCase):
    """A real retry loop stops mutating and building once its first build leaves an unknown closure.

    Each loop runs once over a dedicated fixture. The builder's command owner reports one unknown
    closure instead of starting the child, and Popen refuses and records launches; the retained
    synthetic trees are removed only after zero launches were proven.
    """

    def refuse_launch(self, *arguments, **keywords):
        self.launches.append(arguments)
        raise AssertionError("a real child must not launch in a retry-loop regression")

    def unknown_closure(self, argv, environment, **keywords):
        self.builds.append(argv)
        raise BoundedCommandError(CommandFailure.TIMEOUT)

    def run_loop(self, name):
        self.launches: list[tuple] = []
        self.builds: list[list[str]] = []
        tree = FixtureTree.open(prefix="candidate-loop-fixture", label="candidate loop fixture")
        # Registered before the fixture's own Git, so a failed setup is still disposed and reported.
        self.addCleanup(tree.dispose)
        fixture = support.CandidateFixture(tree)

        class Loop(RefusalTests):
            pass

        Loop.fixture = fixture
        inner = Loop(name)
        self.addCleanup(self.remove_proven, inner, tree)
        result = unittest.TestResult()
        with patch.object(support, "run_bounded_command", side_effect=self.unknown_closure), \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch), \
                contextlib.redirect_stderr(io.StringIO()):
            inner.run(result)
        self.assertEqual(self.launches, [])
        self.assertFalse(result.wasSuccessful())
        first = tree.unresolved
        assert isinstance(first, VerificationError)
        self.assertEqual((first.code, first.stage), ("command_timeout", "fixture-cli"))
        self.assertIs(inner.case.unresolved, first)
        return fixture, inner.case.root

    def remove_proven(self, inner, tree):
        """Dispose both owners; remove a retained one only after zero launches were proven."""
        owners = [owner for owner in (getattr(inner, "case", None), tree) if owner is not None]
        with contextlib.redirect_stderr(io.StringIO()):
            retained = [owner for owner in owners if not owner.dispose()]
        if self.launches:
            return
        for owner in retained:
            owner.remove_retained()

    def test_dirty_source_loop_never_writes_the_source_again_after_an_unknown_build(self):
        fixture, _scratch = self.run_loop("test_dirty_or_untracked_source_is_refused")
        self.assertFalse((fixture.repository / "stray.txt").exists(), "the loop wrote the source again")
        self.assertEqual((fixture.repository / "go.sum").read_text(), "changed\n")
        self.assertEqual(len(self.builds), 1)

    def test_malformed_identity_loop_never_builds_again_after_an_unknown_build(self):
        _fixture, scratch = self.run_loop("test_malformed_identity_and_foreign_root_are_refused_before_any_work")
        self.assertEqual(len(self.builds), 1, "the loop built again beneath an unknown closure")
        self.assertFalse((scratch / "elsewhere").exists())


class RetryLoopSetupFailureTests(unittest.TestCase):
    """run_loop disposes its fixture tree even when the fixture's own Git setup fails.

    A dedicated inner retry-loop case runs the real run_loop and doCleanups. The fixture Git
    command owner reports one failure instead of starting Git, Popen refuses and records launches,
    and FixtureTree.open is passed through so the created tree can be checked; a tree still on
    disk afterwards is removed by its created identity only after zero launches were proven.
    """

    def setUp(self):
        self.baseline = open_descriptors()
        self.launches: list[tuple] = []
        self.trees: list[FixtureTree] = []
        self.addCleanup(self.remove_proven)

    def refuse_launch(self, *arguments, **keywords):
        self.launches.append(arguments)
        raise AssertionError("a real child must not launch in a setup-failure control")

    def remove_proven(self):
        if self.launches:
            return
        for tree in self.trees:
            if tree.root.exists():
                with contextlib.redirect_stderr(io.StringIO()):
                    removed = tree.dispose()
                if not removed:
                    tree.remove_retained()

    def run_loop_setup(self, failure):
        """Run the real run_loop in a dedicated inner case whose fixture Git reports ``failure``."""
        open_tree = FixtureTree.open

        def recorded(*arguments, **keywords):
            tree = open_tree(*arguments, **keywords)
            self.trees.append(tree)
            return tree

        class Loop(RetryLoopReuseRegressionTests):
            def test_setup(inner):
                inner.run_loop("test_dirty_or_untracked_source_is_refused")

        inner = Loop("test_setup")
        result = unittest.TestResult()
        report = io.StringIO()
        with patch.object(FixtureTree, "open", recorded), \
                patch.object(support, "command", side_effect=failure) as owner, \
                patch.object(operations.subprocess, "Popen", side_effect=self.refuse_launch), \
                contextlib.redirect_stderr(report):
            inner.run(result)
        owner.assert_called_once()
        self.assertEqual(self.launches, [])
        self.assertEqual(len(self.trees), 1)
        ((_, trace),) = result.errors
        self.assertIn(failure.code, trace)
        return self.trees[0], report.getvalue()

    def test_closed_setup_failure_removes_the_loop_fixture(self):
        tree, report = self.run_loop_setup(VerificationError("command_start_failed", "fixture-git"))
        self.assertFalse(tree.root.exists(), "a closed setup failure left the loop fixture behind")
        self.assertIsNone(tree.unresolved)
        self.assertNotIn("retained", report)
        self.assertEqual(open_descriptors(), self.baseline)

    def test_unknown_setup_failure_retains_the_loop_fixture_with_its_fact(self):
        failure = VerificationError("command_termination_failed", "fixture-git")
        tree, report = self.run_loop_setup(failure)
        self.assertIn(f"candidate loop fixture retained (command_termination_failed): {tree.root}", report,
                      "an unknown setup failure was not disposed and reported")
        self.assertTrue(tree.root.is_dir())
        self.assertIs(tree.unresolved, failure)
        self.assertEqual(open_descriptors(), self.baseline)


class EnvironmentTests(unittest.TestCase):
    def test_global_git_configuration_is_disabled_with_the_runtime_null_device(self):
        with patch.object(os, "devnull", "fixture-null-device"):
            environment = child_environment({"PATH": "/bin"})
        self.assertEqual(environment["GIT_CONFIG_GLOBAL"], "fixture-null-device")

    def test_child_environment_keeps_only_the_allowlist(self):
        environment = child_environment({"PATH": "/bin", "HOME": "/home/x", **DECOY_VARIABLES})
        self.assertFalse(set(environment) & set(DECOY_VARIABLES))
        self.assertEqual(set(environment), {"PATH", "HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM",
                                            "GIT_NO_REPLACE_OBJECTS", "GIT_TERMINAL_PROMPT", "GOTOOLCHAIN"})
        self.assertEqual(environment["GOTOOLCHAIN"], "local")
        self.assertEqual(environment["GIT_CONFIG_GLOBAL"], os.devnull)
        with self.assertRaises(VerificationError):
            child_environment({"HOME": "/home/x"})

    def test_replace_objects_stay_disabled_whatever_the_parent_sets(self):
        for parent in ({}, {"GIT_NO_REPLACE_OBJECTS": "0"}, {"GIT_NO_REPLACE_OBJECTS": ""}):
            with self.subTest(parent=parent):
                environment = child_environment({"PATH": "/bin", **parent})
                self.assertEqual(environment.get("GIT_NO_REPLACE_OBJECTS"), "1")


class PayloadInventoryTests(unittest.TestCase):
    """The finished payload holds exactly the canonical inventory and its manifest."""

    def setUp(self):
        self.tree = FixtureTree.open(prefix="candidate-payload-inventory", label="candidate payload inventory")
        self.addCleanup(self.tree.dispose)
        output = self.tree.root / "payload"
        output.mkdir()
        request = BuildRequest(version=support.VERSION, commit="a1" * 20, base_commit="c3" * 20,
                               repository="hishamkaram/delegation-layer")
        files = {}
        for path, _kind, _os, _arch in expected_inventory(support.VERSION):
            data = f"{path} payload bytes\n".encode()
            (output / path).write_bytes(data)
            files[path] = (len(data), sha(data))
        inputs = build_inputs(config_sha256="1" * 64,
                              go_sum_sha256="2" * 64, go="1.27.3", goreleaser="2.18.1", python="3.12.13",
                              node="22.11.0")
        self.manifest = encode_manifest(build_manifest(request, tree="b2" * 20, inputs=inputs, files=files))
        (output / MANIFEST_PATH).write_bytes(self.manifest)
        self.context = BuildContext(
            request=request, receipt={}, environment={}, clone=self.tree.root / "clone",
            work=self.tree.root / "work", output=output, owner=DirectoryOwner(),
            goreleaser=self.tree.root / "goreleaser")

    def assert_not_exact(self):
        with self.assertRaises(VerificationError) as caught:
            candidate_build._require_exact_payload(self.context, self.manifest)
        self.assertEqual((caught.exception.code, caught.exception.stage),
                         ("candidate_payload_not_exact", candidate_build.STAGE))

    def test_the_exact_inventory_and_manifest_are_accepted(self):
        self.assertEqual(sorted(path.name for path in self.context.output.iterdir()), PAYLOAD_NAMES)
        candidate_build._require_exact_payload(self.context, self.manifest)

    def test_an_extra_file_is_refused(self):
        (self.context.output / "stray.txt").write_bytes(b"stray\n")
        self.assert_not_exact()

    def test_a_missing_file_is_refused(self):
        archive = next(path for path, kind, _os, _arch in expected_inventory(support.VERSION)
                       if kind is ArtifactKind.ARCHIVE)
        (self.context.output / archive).unlink()
        self.assert_not_exact()


if __name__ == "__main__":
    unittest.main()
