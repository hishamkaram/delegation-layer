"""The candidate install proof application binds its inputs through the canonical owners.

Every repository, locator, served artifact and output directory here is a
test-owned fixture. The full-flow cases run the committed install.sh against a
fixture archive behind the curl shim and a fake ``gh``; no provider or network
is reached, and nothing here is native Homebrew, npm or shell install evidence.
"""

from __future__ import annotations

from collections.abc import Callable
import contextlib
from dataclasses import replace
import errno
import io
import json
import os
import shutil
import tarfile
from pathlib import Path
import sys
import unittest
from unittest import mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts import prove_candidate_install as cli
from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.release_verification import candidate_install_proof as app
from scripts.delegate_acceptance.release_verification import cleanup, commands, filesystem, github, source
from scripts.delegate_acceptance.release_verification.assets import platform_identity
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    CHANNEL_RULES, WITNESS_ROLE_KINDS, InstallChannel,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    CHECKSUMS_PATH, PLATFORMS, ArtifactKind, SourceIdentity, expected_inventory, parse_manifest,
)
from scripts.delegate_acceptance.release_verification.candidate_source_observation import TreeEntry, tree_entries
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.contract import BINARIES, VerificationError, expected_archive
from scripts.delegate_acceptance.release_verification.published_ports import ConsumerPorts
from scripts.test_candidate_artifact_support import ARTIFACT_ID, DOWNLOAD, ENDPOINTS, VERSION, CandidateArtifactCase, manifest_bytes, payload_files
from scripts.test_candidate_install_support import RecordingProbe, fake_binary, observed_children
from scripts.test_candidate_source_support import ChildFault, SourceRepository, settle_retained


REPO_ROOT = Path(__file__).resolve().parent.parent
INSTALL_SH = b"#!/bin/sh\necho committed installer\n"
HARNESS_FILES = {
    source.PARSER_SOURCE: (REPO_ROOT / source.PARSER_SOURCE).read_bytes(),
    "install.sh": INSTALL_SH,
}


class ProofCase(unittest.TestCase):
    """One fixture tree holding one executing checkout whose HEAD commits the harness and install.sh."""

    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="candidate-install-proof", label="candidate install proof")
        self.addCleanup(self.tree.dispose)
        self.repository = SourceRepository(self.tree, "checkout")
        self.head = self.repository.commit(HARNESS_FILES, "harness")
        self.checkout = self.repository.path
        self.manifest = replace(fx.manifest(), source=SourceIdentity(
            commit=self.head, tree=self.repository.tree_of(self.head), base_commit=self.head))

    def call(self, function, *args, **kwargs):
        """Run one application step; each child's original failure reaches the tree before any conversion."""
        with observed_children(self.tree):
            return function(*args, **kwargs)

    def assertRefused(self, code: str, function, *args, **kwargs) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            self.call(function, *args, **kwargs)
        self.assertEqual(caught.exception.code, code)
        return caught.exception

    def request(self, *, output: Path | None = None,
                channel: InstallChannel = InstallChannel.SHELL) -> app.ProofRequest:
        return app.ProofRequest(
            artifact_id=ARTIFACT_ID, channel=channel,
            output_dir=output or self.tree.root / "output", checkout=self.checkout, github_auth="local-config")

    def receipt(self, output: Path) -> dict:
        return json.loads((output / app.RECEIPT_NAME).read_text(encoding="utf-8"))


class ChannelFilesTests(ProofCase):
    def test_each_channel_supplies_exactly_its_consumed_roles_with_the_host_archive(self) -> None:
        payload = self.tree.root / "payload"
        platform = PLATFORMS[0]
        for channel in InstallChannel:
            with self.subTest(channel=channel):
                files = app.channel_files(self.manifest, payload, channel, platform)
                consumed = CHANNEL_RULES[channel].consumed
                self.assertEqual({role for role, _ in files.roles()},
                                 {role for role, kind in WITNESS_ROLE_KINDS.items() if kind in consumed})
                self.assertEqual(files.archive, payload / self.manifest.archive(*platform).path)
        self.assertNotIn("package", dict(app.channel_files(self.manifest, payload, InstallChannel.BREW,
                                                           ("Darwin", "arm64")).roles()))

    def test_an_unlisted_host_platform_refuses(self) -> None:
        self.assertRefused("candidate_platform_unknown", app.channel_files, self.manifest,
                           self.tree.root, InstallChannel.SHELL, ("Linux", "riscv64"))

    def test_operator_channel_names_map_to_contract_channels(self) -> None:
        self.assertEqual(app.CHANNELS, {"shell": InstallChannel.SHELL, "npm": InstallChannel.NPM,
                                        "homebrew": InstallChannel.BREW})


class InstallerSourceTests(ProofCase):
    def read(self, manifest=None) -> bytes:
        return self.call(app.installer_source, {"commands": []}, checkout=self.checkout,
                         manifest=manifest or self.manifest)

    def test_the_source_commit_blob_wins_over_the_working_tree(self) -> None:
        (self.checkout / "install.sh").write_bytes(b"#!/bin/sh\necho substituted\n")
        self.assertEqual(self.read(), INSTALL_SH)

    def test_the_manifest_commit_selects_the_bytes_not_head(self) -> None:
        later = self.repository.commit({"install.sh": b"#!/bin/sh\necho later\n"}, "later")
        self.assertNotEqual(later, self.head)
        self.assertEqual(self.read(), INSTALL_SH)

    def test_a_committed_symlink_installer_refuses(self) -> None:
        (self.checkout / "install.sh").unlink()
        (self.checkout / "install.sh").symlink_to("scripts")
        self.repository.git("add", "--all")
        self.repository.git("commit", "--quiet", "-m", "symlink")
        commit = self.repository.git("rev-parse", "HEAD")
        manifest = replace(self.manifest, source=SourceIdentity(
            commit=commit, tree=self.repository.tree_of(commit), base_commit=commit))
        with self.assertRaises(VerificationError) as caught:
            self.read(manifest)
        self.assertEqual(caught.exception.code, "registry_file_not_regular")

    def test_a_source_commit_without_local_objects_refuses_without_fallback(self) -> None:
        manifest = replace(self.manifest, source=SourceIdentity(
            commit="1" * 40, tree=self.manifest.source.tree, base_commit="1" * 40))
        with self.assertRaises(VerificationError) as caught:
            self.read(manifest)
        self.assertNotIn(caught.exception.code, {"registry_file_missing", "registry_file_not_regular"})


class OutputDirectoryTests(ProofCase):
    def test_unownable_output_directories_refuse_before_anything_is_created(self) -> None:
        existing = self.tree.root / "existing"
        existing.mkdir()
        cases = {
            "candidate_install_output_invalid": self.request(output=Path("relative")),
            "candidate_install_output_exists": self.request(output=existing),
            "candidate_install_output_overlap": self.request(output=self.checkout / "output"),
        }
        for code, request in cases.items():
            with self.subTest(code=code):
                self.assertRefused(code, app.prove_candidate_install, request)
        self.assertFalse((self.checkout / "output").exists())
        self.assertEqual(list(existing.iterdir()), [])

class BootstrapParentTests(ProofCase):
    """The bootstrap root's parent is resolved once at the application boundary; HOME stays the default."""

    def setUp(self) -> None:
        super().setUp()
        self.home = self.tree.root / "home"
        self.home.mkdir()
        environment = mock.patch.dict(os.environ, {"HOME": str(self.home)})
        environment.start()
        self.addCleanup(environment.stop)

    def parse(self, *extra: str) -> app.ProofRequest:
        """The request the real contributor CLI builds, captured before any proof step runs."""
        requests: list[app.ProofRequest] = []
        with mock.patch.object(cli, "prove_candidate_install", lambda request: requests.append(request) or 0):
            self.assertEqual(cli.main(["--artifact-id", str(ARTIFACT_ID), "--channel", "shell",
                                       "--output-dir", str(self.tree.root / "output"), *extra]), 0)
        (request,) = requests
        return request

    def test_the_cli_passes_only_an_explicit_parent_and_the_default_resolves_home(self) -> None:
        self.assertIsNone(self.parse().bootstrap_parent)
        self.assertEqual(app.require_bootstrap_parent(self.request()), self.home.resolve())
        short = self.tree.root / "short"
        short.mkdir()
        (self.tree.root / "link").symlink_to(short)
        request = self.parse("--bootstrap-parent", str(self.tree.root / "link"))
        self.assertEqual(request.bootstrap_parent, self.tree.root / "link")
        self.assertEqual(app.require_bootstrap_parent(request), short.resolve())

    def test_an_invalid_parent_refuses_before_the_output_or_the_probe_exists(self) -> None:
        a_file = self.tree.root / "a-file"
        a_file.write_text("not a directory\n")
        cases = {"relative": Path("short"), "dotdot": self.home / ".." / "home",
                 "missing": self.tree.root / "missing", "file": a_file}
        probe = mock.Mock()
        with mock.patch.object(app, "InstalledBootstrapProbe", probe):
            for name, parent in cases.items():
                with self.subTest(case=name):
                    request = self.parse("--bootstrap-parent", str(parent))
                    self.assertRefused("candidate_install_bootstrap_parent_invalid",
                                       app.prove_candidate_install, request)
                    self.assertFalse(request.output_dir.exists())
        probe.assert_not_called()


class ExportTests(ProofCase):
    def test_export_reads_back_the_exact_new_bytes(self) -> None:
        path = self.tree.root / "proof.json"
        self.assertEqual(app.export_file(path, b'{"proof":1}\n'), b'{"proof":1}\n')
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)

    def test_an_existing_target_refuses_and_keeps_its_bytes(self) -> None:
        path = self.tree.root / "proof.json"
        path.write_bytes(b"prior")
        self.assertRefused("candidate_install_evidence_unwritable", app.export_file, path, b"new")
        self.assertEqual(path.read_bytes(), b"prior")

    def test_a_write_fault_is_reported_not_swallowed(self) -> None:
        path = self.tree.root / "proof.json"
        with mock.patch.object(filesystem, "write_all", side_effect=OSError(28, "no space")):
            self.assertRefused("candidate_install_evidence_write_failed", app.export_file, path, b"data")

    def test_an_interrupted_write_closes_once_keeps_the_interrupt_and_reads_nothing_back(self) -> None:
        path, interrupt, created, closes = self.tree.root / "proof.json", KeyboardInterrupt(), [], []
        real_create, real_close = filesystem.create_new_file, os.close

        def create(target, code, *, stage):
            created.append(real_create(target, code, stage=stage))
            return created[-1]

        def close(descriptor):
            if descriptor in created:
                closes.append(descriptor)
            real_close(descriptor)

        with mock.patch.object(filesystem, "create_new_file", side_effect=create), \
                mock.patch.object(filesystem.os, "close", side_effect=close), \
                mock.patch.object(filesystem, "write_all", side_effect=interrupt), \
                mock.patch.object(app, "read_bounded_regular_file") as readback, \
                self.assertRaises(KeyboardInterrupt) as caught:
            app.export_file(path, b"data")
        self.assertIs(caught.exception, interrupt)
        self.assertEqual((len(closes), readback.call_count), (1, 0))
        with self.assertRaises(OSError) as closed:
            os.fstat(closes[0])
        self.assertEqual(closed.exception.errno, errno.EBADF)


class GateTests(ProofCase):
    def test_an_unproven_github_child_retains_the_workspace_and_reports_it(self) -> None:
        output = self.tree.root / "output"
        fault = ChildFault(lambda argv: argv[:1] == ["gh"])
        with mock.patch.object(commands, "run_bounded_command", fault), \
                mock.patch.object(github, "run_bounded_command", fault):
            code = self.call(app.prove_candidate_install, self.request())
        # The tree observed the original fault before the gate converted it into the receipt.
        self.assertIs(type(self.tree.unresolved), BoundedCommandError)
        assert isinstance(self.tree.unresolved, BoundedCommandError)
        self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
        self.assertEqual(code, 1)
        receipt = self.receipt(output)
        failure = receipt["failure"]
        self.assertEqual((failure["code"], failure["stage"]), ("command_timeout", "github-api"))
        workspace = output / app.WORKSPACE_NAME
        self.assertEqual(receipt["candidate_install"]["unresolved_ownership"]["workspace"], str(workspace))
        self.assertIn(str(workspace.resolve()), receipt["cleanup"]["retained_paths"])
        settle_retained(self, self.tree, fault, (workspace, output / app.RECEIPT_NAME))


def host_archive(binaries: dict[str, bytes]) -> bytes:
    """A release-shaped gzip tar of the four binaries, as the candidate build ships it."""
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz") as bundle:
        for name, data in binaries.items():
            member = tarfile.TarInfo(name)
            member.size, member.mode = len(data), 0o755
            bundle.addfile(member, io.BytesIO(data))
    return output.getvalue()


class FullFlowTests(CandidateArtifactCase):
    """One finite shell-channel run: served candidate, committed install.sh, real installer children.

    The source commit carries install.sh; HEAD, the executing harness, is a later
    commit; the manifest records a third, build harness fingerprint.
    """

    def setUp(self) -> None:
        super().setUp()
        self.repository = SourceRepository(self.tree, "checkout")
        self.source_commit = self.repository.commit(
            {"install.sh": (REPO_ROOT / "install.sh").read_bytes()}, "source")
        self.head = self.source_commit
        info = platform_identity()
        archive_name = expected_archive(info, "v" + VERSION)
        archives = {path: host_archive({name: fake_binary(name, VERSION) for name in BINARIES})
                    if path == archive_name else f"{path} other platform\n".encode()
                    for path, kind, _os, _arch in expected_inventory(VERSION) if kind is ArtifactKind.ARCHIVE}
        checksums = "".join(f"{sha(data)}  {path}\n" for path, data in sorted(archives.items())).encode()
        files = payload_files(**archives, **{CHECKSUMS_PATH: checksums})
        manifest = manifest_bytes(files, commit=self.source_commit)

        def head_sha(replies: dict) -> None:
            replies["artifact"]["workflow_run"]["head_sha"] = self.source_commit
            replies["attempt"]["head_sha"] = self.source_commit

        self.served = self.serve(files=files, manifest=manifest, change=head_sha)
        self.manifest = parse_manifest(manifest)
        self.output = self.root / "output"
        # The operator HOME below which the bootstrap probe creates its short private root.
        self.home = self.root / "home"
        self.home.mkdir()
        # Unit isolation: the version-only fake binaries cannot run the installed fixture turn.
        self.probe: Callable[..., object] | None = lambda **_owners: RecordingProbe()
        self.events: list[str] = []
        self.at_cleanup: dict[str, bool] = {}
        self.after: Callable[[str], None] | None = None

    def run_child(self, receipt: dict, name: str, argv: list[str], env: dict[str, str], **options) -> bytes:
        """The installer's children; ``prove`` routes their launches, like every other child's, through the tree.

        The ``after`` hook mutates the run's files, so it never runs while the tree holds an unresolved child.
        """
        try:
            return command(receipt, name, argv, env, **options)
        finally:
            if self.after is not None and self.tree.unresolved is None:
                self.after(name)

    def spy(self, owner, name: str, event: str):
        real = getattr(owner, name)

        def recorded(*args, **kwargs):
            self.events.append(event)
            return real(*args, **kwargs)
        return mock.patch.object(owner, name, side_effect=recorded)

    def prove(self, *, bootstrap_parent: Path | None = None) -> int:
        real_prepare = cleanup.prepare_directory_cleanup

        def prepare(*args, **kwargs):
            self.events.append("cleanup")
            self.at_cleanup.update({name: (self.output / name).exists()
                                    for name in (app.WORKSPACE_NAME,)})
            return real_prepare(*args, **kwargs)

        environment = {"PATH": os.pathsep.join([str(self.root / "bin"), os.environ["PATH"]]),
                       "FIXTURE_LOG": str(self.log_path), "FIXTURE_ROUTES": str(self.routes_path),
                       "GH_TOKEN": "private-canary", "HOME": str(self.home)}
        request = app.ProofRequest(artifact_id=ARTIFACT_ID, channel=InstallChannel.SHELL, output_dir=self.output,
                                   checkout=self.repository.path, bootstrap_parent=bootstrap_parent)
        ports = ConsumerPorts(run=self.run_child, which=shutil.which)
        probe = (contextlib.nullcontext() if self.probe is None
                 else mock.patch.object(app, "InstalledBootstrapProbe", self.probe))
        with mock.patch.dict(os.environ, environment), observed_children(self.tree), probe, \
                self.spy(app, "export_file", "export"), \
                mock.patch.object(cleanup, "prepare_directory_cleanup", side_effect=prepare), \
                contextlib.redirect_stderr(io.StringIO()):
            return app.prove_candidate_install(request, ports=ports)

    def receipt(self) -> dict:
        return json.loads((self.output / app.RECEIPT_NAME).read_text(encoding="utf-8"))

    def test_a_verified_install_records_actual_facts_and_cleans_the_workspace(self) -> None:
        self.assertEqual(self.prove(), 0)
        self.assertEqual(self.events, ["export", "cleanup"])
        self.assertEqual(self.at_cleanup, {app.WORKSPACE_NAME: True})
        self.assertFalse((self.output / app.WORKSPACE_NAME).exists())
        receipt = self.receipt()
        self.assertEqual((receipt["verdict"], receipt["cleanup"]["status"]), ("pass", "complete"))
        facts = receipt["candidate_install"]
        self.assertEqual((facts["status"], facts["artifact_id"]), ("verified", ARTIFACT_ID))
        self.assertEqual(facts["installation"]["tag"], self.manifest.build_tag)
        self.assertEqual(self.requested(), [*ENDPOINTS.values(), DOWNLOAD])
        self.assertNotIn("private-canary", json.dumps(receipt))

    def test_the_real_probe_refuses_the_version_only_install_and_retains_its_root(self) -> None:
        self.probe = None
        self.assertEqual(self.prove(), 1)
        self.assertRetained("candidate_bootstrap_unresolved")
        receipt = self.receipt()
        bootstrap = receipt["candidate_install"]["bootstrap"]
        root = Path(bootstrap["root"])
        self.assertEqual(root.parent, self.home.resolve())
        self.assertIn(str(root), receipt["cleanup"]["retained_paths"])
        self.assertEqual((bootstrap["status"], bootstrap["failure"]["type"]), ("failed", "RuntimeError"))
        self.assertEqual(len(bootstrap["unresolved_ownership"]), 1)
        self.assertNotIn("private-canary", json.dumps(receipt))
        # The test's own proof before the tree removes the root: the stand-in exited 8 and left no state.
        dispatch = json.loads((root / "r" / "packaged-dispatch.json").read_text(encoding="utf-8"))
        self.assertEqual(dispatch["exit_code"], 8)
        self.assertEqual(list((root / "h").iterdir()), [])

    def test_a_selected_parent_holds_the_real_probe_root_and_the_operator_home_stays_untouched(self) -> None:
        selected = self.root / "s"
        selected.mkdir()
        self.probe = None
        self.assertEqual(self.prove(bootstrap_parent=selected), 1)
        self.assertRetained("candidate_bootstrap_unresolved")
        receipt = self.receipt()
        root = Path(receipt["candidate_install"]["bootstrap"]["root"])
        self.assertEqual((root.parent, receipt["candidate_install"]["bootstrap_parent"]),
                         (selected.resolve(), str(selected.resolve())))
        self.assertIn(str(root), receipt["cleanup"]["retained_paths"])
        self.assertEqual(list(self.home.iterdir()), [])

    def assertRetained(self, code: str) -> None:
        receipt = self.receipt()
        workspace = self.output / app.WORKSPACE_NAME
        self.assertEqual((receipt["verdict"], receipt["failure"]["code"]), ("fail", code))
        self.assertEqual(receipt["cleanup"]["status"], "retained-for-investigation")
        self.assertTrue(workspace.is_dir())
        self.assertIn(str(workspace.resolve()), receipt["cleanup"]["retained_paths"])
        self.assertNotIn("cleanup", self.events)
        self.assertNotEqual(receipt["candidate_install"]["status"], "verified")

    def assertUnprovenChildRetained(self, fault: ChildFault, stage: str) -> None:
        """One faulted child: the tree saw the original, the gate retained, then the test's proof removes."""
        with mock.patch.object(commands, "run_bounded_command", fault), \
                mock.patch.object(github, "run_bounded_command", fault):
            code = self.prove()
        self.assertIs(type(self.tree.unresolved), BoundedCommandError)
        assert isinstance(self.tree.unresolved, BoundedCommandError)
        self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
        self.assertEqual(code, 1)
        self.assertRetained("command_timeout")
        self.assertEqual(self.receipt()["failure"]["stage"], stage)
        settle_retained(self, self.tree, fault, (self.output / app.WORKSPACE_NAME, self.output / app.RECEIPT_NAME))

    def test_an_unproven_default_path_child_retains_the_workspace_and_the_tree(self) -> None:
        fault = ChildFault(lambda argv: argv[:1] == ["delegate"])
        self.assertUnprovenChildRetained(fault, "delegate-version")
        self.assertGreater(fault.launched, 0)

    def test_an_unproven_github_child_retains_the_workspace_and_the_tree(self) -> None:
        self.assertUnprovenChildRetained(ChildFault(lambda argv: argv[:1] == ["gh"]), "github-api")

    def test_a_mutation_hook_never_runs_after_an_unproven_installer_child(self) -> None:
        hooks: list[str] = []
        later = self.repository.path / "later.txt"

        def mutate(name: str) -> None:
            hooks.append(name)
            later.write_bytes(b"later\n")
        self.after = mutate
        shell = shutil.which("sh")
        fault = ChildFault(lambda argv: argv[:1] == [shell])
        with mock.patch.object(commands, "run_bounded_command", fault), \
                mock.patch.object(github, "run_bounded_command", fault):
            code = self.prove()
        receipt, mutated = self.receipt(), later.exists()
        settle_retained(self, self.tree, fault, (self.output / app.WORKSPACE_NAME, self.output / app.RECEIPT_NAME))
        self.assertEqual((hooks, mutated), ([], False))
        self.assertIs(type(self.tree.unresolved), BoundedCommandError)
        assert isinstance(self.tree.unresolved, BoundedCommandError)
        self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
        self.assertEqual(code, 1)
        self.assertEqual((receipt["failure"]["code"], receipt["failure"]["stage"]),
                         ("command_timeout", "candidate-shell-install"))

class ImportClosureTests(unittest.TestCase):
    def test_artifact_kinds_of_every_channel_installer_are_consumed(self) -> None:
        for rule in CHANNEL_RULES.values():
            self.assertTrue(rule.installer_kind is None or rule.installer_kind in rule.consumed)
        self.assertIs(WITNESS_ROLE_KINDS["formula"], ArtifactKind.HOMEBREW_FORMULA)


class TreeEntryTests(unittest.TestCase):
    def assertCode(self, code, function, *args):
        with self.assertRaises(VerificationError) as caught:
            function(*args)
        self.assertEqual(caught.exception.code, code)

    def test_entries_decode_exact_paths_and_modes(self):
        oid = "a" * 40
        output = (f"100644 blob {oid}\t{"file.json"}\x00100755 blob {oid}\tbin/tool\0"
                  f"120000 blob {oid}\tlink.json\x00040000 tree {oid}\tdir\x00160000 commit {oid}\tmodule\0"
                  f"100644 blob {oid}\tname\nwith newline\0").encode()
        entries = tree_entries(output)
        self.assertEqual(entries["file.json"], TreeEntry(mode="100644", kind="blob", object_id=oid))
        self.assertEqual({path for path, entry in entries.items() if entry.regular_blob},
                         {"file.json", "bin/tool", "name\nwith newline"})
        self.assertEqual(tree_entries(b""), {})

    def test_malformed_listing_is_refused(self):
        oid = "a" * 40
        for output, code in ((f"100644 blob {oid}\ta".encode(), "git_output_invalid"),
                             (f"100644 blob {oid}\ta\x00100644 blob {oid}\ta\0".encode(), "git_output_invalid"),
                             (f"100644 blob {oid[:39]}\ta\0".encode(), "git_output_invalid"),
                             (f"10064 blob {oid}\ta\0".encode(), "git_output_invalid"),
                             (f"100644 blob {oid}\tbad".encode() + b"\xff\0", "source_path_not_utf8")):
            with self.subTest(output=output):
                self.assertCode(code, tree_entries, output)


if __name__ == "__main__":
    unittest.main()
