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
from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.release_verification import candidate_install_proof as app
from scripts.delegate_acceptance.release_verification import cleanup, commands, filesystem, github, source
from scripts.delegate_acceptance.release_verification.assets import platform_identity
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    CHANNEL_RULES, WITNESS_ROLE_KINDS, HarnessIdentity, InstallChannel, SourceFileBinding, read_install_proof,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    CHECKSUMS_PATH, PLATFORMS, ArtifactKind, SourceIdentity, encode_locator, expected_inventory, parse_manifest,
)
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.contract import BINARIES, VerificationError, expected_archive
from scripts.delegate_acceptance.release_verification.published_ports import ConsumerPorts
from scripts.test_candidate_artifact_support import (
    DOWNLOAD, ENDPOINTS, REPOSITORY, VERSION, CandidateArtifactCase, manifest_bytes, payload_files,
)
from scripts.test_candidate_install_support import fake_binary, observed_children
from scripts.test_candidate_source_support import ChildFault, SourceRepository, settle_retained


REPO_ROOT = Path(__file__).resolve().parent.parent
INSTALL_SH = b"#!/bin/sh\necho committed installer\n"
HARNESS_FILES = {
    source.PARSER_SOURCE: (REPO_ROOT / source.PARSER_SOURCE).read_bytes(),
    app.CLI_SEED: b"VALUE = 'executing harness'\n",
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

    def request(self, *, output: Path | None = None, locator: Path | None = None,
                channel: InstallChannel = InstallChannel.SHELL) -> app.ProofRequest:
        return app.ProofRequest(
            locator=locator or self.tree.root / "locator.json", channel=channel,
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


class HarnessTests(ProofCase):
    def observe(self) -> object:
        return self.call(app.executing_harness, {"commands": []}, self.checkout)

    def test_a_committed_harness_differing_from_the_build_harness_is_accepted(self) -> None:
        identity = self.observe()
        self.assertEqual(identity.commit, self.head)
        self.assertEqual(identity.sha256, source.fingerprint_sources(self.checkout, (app.CLI_SEED,))["sha256"])
        self.assertNotEqual((identity.commit, identity.sha256),
                            (self.manifest.build.harness_commit, self.manifest.build.harness_sha256))

    def test_a_working_tree_harness_mutation_refuses(self) -> None:
        (self.checkout / app.CLI_SEED).write_bytes(b"VALUE = 'substituted'\n")
        self.assertRefused("candidate_install_harness_not_committed", app.executing_harness,
                           {"commands": []}, self.checkout)

    def test_a_replacement_object_cannot_make_substituted_bytes_committed(self) -> None:
        substituted = b"VALUE = 'substituted'\n"
        original = self.repository.git("rev-parse", f"{self.head}:{app.CLI_SEED}")
        (self.checkout / app.CLI_SEED).write_bytes(substituted)
        replacement = self.repository.git("hash-object", "-w", app.CLI_SEED)
        self.repository.git("replace", original, replacement)
        self.assertRefused("candidate_install_harness_not_committed", app.executing_harness,
                           {"commands": []}, self.checkout)


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

    def test_a_locator_inside_the_output_directory_overlaps(self) -> None:
        output = self.tree.root / "output"
        self.assertRefused("candidate_install_output_overlap", app.require_output_dir,
                           self.request(output=output, locator=output / "locator.json"))


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
    def write_locator(self) -> Path:
        path = self.tree.root / "locator.json"
        path.write_bytes(encode_locator(fx.locator_for(fx.manifest())))
        return path

    def test_an_uncommitted_harness_fails_before_any_workspace_or_github_request(self) -> None:
        (self.checkout / app.CLI_SEED).write_bytes(b"VALUE = 'substituted'\n")
        output = self.tree.root / "output"
        self.assertEqual(self.call(app.prove_candidate_install, self.request(locator=self.write_locator())), 1)
        receipt = self.receipt(output)
        self.assertEqual(receipt["failure"]["code"], "candidate_install_harness_not_committed")
        self.assertEqual((receipt["verdict"], receipt["cleanup"]["status"]), ("fail", "retained-for-investigation"))
        self.assertFalse((output / app.WORKSPACE_NAME).exists())
        self.assertFalse(any(record["name"] == "github-api" for record in receipt["commands"]))

    def test_an_unreadable_locator_fails_with_the_committed_harness_recorded(self) -> None:
        output = self.tree.root / "output"
        self.assertEqual(self.call(app.prove_candidate_install, self.request()), 1)
        receipt = self.receipt(output)
        self.assertEqual(receipt["failure"]["code"], "candidate_locator_unreadable")
        self.assertEqual(receipt["source_harness"]["revision"], self.head)
        self.assertEqual(receipt["claim"], app.CLAIM)
        self.assertEqual(receipt["gate"], "candidate-install-shell")

    def test_an_unproven_github_child_retains_the_workspace_and_reports_it(self) -> None:
        output = self.tree.root / "output"
        fault = ChildFault(lambda argv: argv[:1] == ["gh"])
        with mock.patch.object(commands, "run_bounded_command", fault), \
                mock.patch.object(github, "run_bounded_command", fault):
            code = self.call(app.prove_candidate_install, self.request(locator=self.write_locator()))
        # The tree observed the original fault before the gate converted it into the receipt.
        self.assertIs(type(self.tree.unresolved), BoundedCommandError)
        self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
        self.assertEqual(code, 1)
        receipt = self.receipt(output)
        failure = receipt["failure"]
        self.assertEqual((failure["code"], failure["stage"]), ("command_timeout", "github-api"))
        workspace = output / app.WORKSPACE_NAME
        self.assertEqual(receipt["candidate_install"]["unresolved_ownership"]["workspace"], str(workspace))
        self.assertIn(str(workspace.resolve()), receipt["cleanup"]["retained_paths"])
        self.assertFalse((output / app.PROOF_NAME).exists())
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
        self.head = self.repository.commit(
            {source.PARSER_SOURCE: HARNESS_FILES[source.PARSER_SOURCE], app.CLI_SEED: HARNESS_FILES[app.CLI_SEED]},
            "executing harness")
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
        self.locator_path = self.root / "locator.json"
        self.locator_path.write_bytes(encode_locator(self.served.locator))
        self.output = self.root / "output"
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

    def prove(self) -> int:
        real_prepare = cleanup.prepare_directory_cleanup

        def prepare(*args, **kwargs):
            self.events.append("cleanup")
            self.at_cleanup.update({name: (self.output / name).exists()
                                    for name in (app.PROOF_NAME, app.WITNESS_NAME, app.WORKSPACE_NAME)})
            return real_prepare(*args, **kwargs)

        environment = {"PATH": os.pathsep.join([str(self.root / "bin"), os.environ["PATH"]]),
                       "FIXTURE_LOG": str(self.log_path), "FIXTURE_ROUTES": str(self.routes_path),
                       "GH_TOKEN": "private-canary"}
        request = app.ProofRequest(locator=self.locator_path, channel=InstallChannel.SHELL, output_dir=self.output,
                                   checkout=self.repository.path)
        ports = ConsumerPorts(run=self.run_child, which=shutil.which)
        with mock.patch.dict(os.environ, environment), observed_children(self.tree), \
                self.spy(app, "executing_harness", "harness"), self.spy(app, "export_file", "export"), \
                self.spy(app, "read_install_proof", "readback"), \
                mock.patch.object(cleanup, "prepare_directory_cleanup", side_effect=prepare), \
                contextlib.redirect_stderr(io.StringIO()):
            return app.prove_candidate_install(request, ports=ports)

    def receipt(self) -> dict:
        return json.loads((self.output / app.RECEIPT_NAME).read_text(encoding="utf-8"))

    def test_a_proven_install_reads_back_and_rechecks_the_harness_before_cleanup(self) -> None:
        self.assertEqual(self.prove(), 0)
        # The harness recheck precedes the proof pair; the pair's readback precedes cleanup.
        self.assertEqual(self.events, ["harness", "export", "harness", "export", "export", "readback", "cleanup"])
        self.assertEqual(self.at_cleanup, {app.PROOF_NAME: True, app.WITNESS_NAME: True, app.WORKSPACE_NAME: True})
        self.assertFalse((self.output / app.WORKSPACE_NAME).exists())
        proof, witness = ((self.output / name).read_bytes() for name in (app.PROOF_NAME, app.WITNESS_NAME))
        value = read_install_proof(proof, witness, manifest=self.manifest,
                                   installer_source=(REPO_ROOT / "install.sh").read_bytes(), repository=REPOSITORY)
        executing = HarnessIdentity(
            commit=self.head, sha256=source.fingerprint_sources(self.repository.path, (app.CLI_SEED,))["sha256"])
        self.assertEqual(value.harness, executing)
        self.assertNotEqual(executing.sha256, self.manifest.build.harness_sha256)
        self.assertNotEqual(self.head, self.source_commit)
        self.assertEqual(value.installer, SourceFileBinding(
            source_commit=self.source_commit, repo_path="install.sh",
            sha256=sha((REPO_ROOT / "install.sh").read_bytes())))
        self.assertEqual((value.channel, value.candidate_id), (InstallChannel.SHELL, self.served.locator.candidate_id))
        receipt = self.receipt()
        self.assertEqual((receipt["verdict"], receipt["cleanup"]["status"]), ("pass", "complete"))
        self.assertEqual(receipt["candidate_install"]["proof"]["sha256"], sha(proof))
        self.assertEqual(receipt["candidate_install"]["witness"]["sha256"], sha(witness))
        self.assertEqual(self.requested(), [*ENDPOINTS.values(), DOWNLOAD])
        self.assertNotIn("private-canary", json.dumps(receipt))

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
        self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
        self.assertEqual(code, 1)
        self.assertEqual((receipt["failure"]["code"], receipt["failure"]["stage"]),
                         ("command_timeout", "candidate-shell-install"))

    def test_a_harness_commit_during_the_run_retains_the_workspace(self) -> None:
        def commit_once(name: str) -> None:
            if name == "candidate-shell-install" and self.after is not None:
                self.after = None
                self.repository.commit({"later.txt": b"later\n"}, "later")
        self.after = commit_once
        self.assertEqual(self.prove(), 1)
        self.assertRetained("candidate_install_harness_changed")
        # Only install.sh was exported; no consumable proof names the changed harness.
        self.assertEqual(self.events, ["harness", "export", "harness"])
        self.assertFalse((self.output / app.PROOF_NAME).exists())
        self.assertFalse((self.output / app.WITNESS_NAME).exists())

    def test_an_interrupted_proof_export_escapes_unchanged_after_retaining_the_workspace(self) -> None:
        interrupt, real_write, writes = KeyboardInterrupt(), filesystem.write_all, []

        def write(descriptor: int, data: bytes) -> None:
            writes.append(len(data))
            if len(writes) == 2:
                raise interrupt
            real_write(descriptor, data)
        with mock.patch.object(filesystem, "write_all", side_effect=write), \
                self.assertRaises(KeyboardInterrupt) as caught:
            self.prove()
        self.assertIs(caught.exception, interrupt)
        self.assertFalse(hasattr(interrupt, "__notes__"))
        self.assertRetained("verification_interrupted")
        self.assertEqual(self.receipt()["failure"]["stage"], app.EXPORT_STAGE)
        self.assertEqual(self.events, ["harness", "export", "harness", "export"])
        self.assertFalse((self.output / app.WITNESS_NAME).exists())
        # The gate owner's pins and the receipt lock are proven released by the case's final descriptor check.

    def test_an_export_fault_retains_the_workspace_and_reads_nothing_back(self) -> None:
        def occupy(name: str) -> None:
            if name == "candidate-shell-install":
                (self.output / app.PROOF_NAME).write_bytes(b"foreign")
        self.after = occupy
        self.assertEqual(self.prove(), 1)
        self.assertRetained("candidate_install_evidence_unwritable")
        self.assertNotIn("readback", self.events)
        self.assertEqual((self.output / app.PROOF_NAME).read_bytes(), b"foreign")

    def test_a_changed_readback_retains_the_workspace(self) -> None:
        real = app.read_bounded_regular_file

        def changed(path: Path, limit: int) -> bytes:
            data = real(path, limit)
            return data[:-1] + b" " if path.name == app.WITNESS_NAME else data
        with mock.patch.object(app, "read_bounded_regular_file", side_effect=changed):
            self.assertEqual(self.prove(), 1)
        self.assertRetained("candidate_install_evidence_readback_failed")
        self.assertNotIn("readback", self.events)


class ImportClosureTests(unittest.TestCase):
    def test_the_application_imports_no_test_fixture_store_or_readiness_owner(self) -> None:
        files = {row["path"] for row in source.fingerprint_sources(REPO_ROOT, (app.CLI_SEED,))["files"]}
        self.assertIn("scripts/delegate_acceptance/release_verification/candidate_install_proof.py", files)
        forbidden = ("scripts/acceptance_tests/", "candidate_store", "candidate_readiness", "scripts/test_")
        self.assertEqual([path for path in files if any(part in path for part in forbidden)], [])

    def test_artifact_kinds_of_every_channel_installer_are_consumed(self) -> None:
        for rule in CHANNEL_RULES.values():
            self.assertTrue(rule.installer_kind is None or rule.installer_kind in rule.consumed)
        self.assertIs(WITNESS_ROLE_KINDS["formula"], ArtifactKind.HOMEBREW_FORMULA)


if __name__ == "__main__":
    unittest.main()
