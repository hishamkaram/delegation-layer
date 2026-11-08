"""Installed providers carry optional diagnostics, never byte attestations."""

from __future__ import annotations

from argparse import Namespace
from collections.abc import Mapping
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from typing import TypedDict
from unittest.mock import Mock, patch

from scripts.acceptance.testing.record_fixtures import object_at
from scripts.acceptance.evidence.digests import sha
from scripts.acceptance.evidence.encoding import canonical_go_json
from scripts.acceptance.evidence.files import write_json
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.provider_drivers.agy import evidence as agy_evidence
from scripts.acceptance.provider_drivers.agy import runner as agy_runner
from scripts.acceptance.provider_drivers.agy.constants import MODE as AGY_MODE
from scripts.acceptance.provider_drivers.claude import evidence as claude_evidence
from scripts.acceptance.provider_drivers.claude import runner as claude_runner
from scripts.acceptance.provider_drivers.claude import runtime as claude_runtime
from scripts.acceptance.provider_drivers.codex import evidence as codex_evidence
from scripts.acceptance.provider_drivers.codex import runner as codex_runner
from scripts.acceptance.provider_drivers.codex import runtime as codex_runtime


ROOT = "a" * 32
TASK = "b" * 32


def metadata(workspace: Path, *, pending: bool = False) -> dict[str, object]:
    value = {
        "root_id": ROOT, "task_id": TASK, "spec_sha256": "c" * 64,
        "provider_executable": "/installed/old-provider", "provider_version": "",
        "containment": "read-only", "approval": "plan",
        "effective_config": {
            "containment": "read-only", "approval": "plan", "digest": "d" * 64,
            "policy": {"workspace": str(workspace), "runtime_sha256": "",
                       "profile_revision": "native-permissions-v1"}},
        "predicate": {"adapter": "fixture", "mode": "read-only"},
    }
    if pending:
        value.update(runtime_preflight_sha256="e" * 64, environment_recorded=True)
    return value


def validate_meta(kind: str, directory: Path, meta: dict[str, object]) -> None:
    write_json(directory / "meta.json", meta, replace=True)
    if kind == "agy":
        object_at(meta, "effective_config")["containment"] = AGY_MODE
        agy_evidence.validate_effective_policy("fixture", meta, directory)
        return
    module = claude_evidence if kind == "claude" else codex_evidence
    arguments = {
        "name": "fixture", "task": TASK, "directory": directory, "root_id": ROOT,
        "workspace": directory,
        "mode": "read-only", "approval": "plan", "revision": "native-permissions-v1",
    }
    module.validate_task_meta(**arguments)


def publication_records(directory: Path, meta: dict[str, object],
                        identity: Mapping[str, object]) -> dict[str, object]:
    write_json(directory / "meta.json", meta)
    bound = {"root_id": ROOT, "task_id": TASK, "spec_sha256": meta["spec_sha256"],
             "meta_sha256": sha((directory / "meta.json").read_bytes())}
    write_json(directory / "submit.json", {
        **bound, "label": "fixture", "supervisor": {
            "config_path": "/owned/pueue.yml", "observed_version": "pueue fixture",
            "client_executable": "/owned/pueue"}})
    write_json(directory / "provider.start", {**bound, "budget_nanos": 1})
    write_json(directory / "provider.started.json", {
        **bound, **identity, "diagnostic_nanos": 0})
    raw = directory / "raw"
    raw.mkdir(exist_ok=True)
    manifest = []
    for name in ("codex-last-message.txt", "stderr", "stdout"):
        (raw / name).write_bytes(b"")
        manifest.append({"path": "raw/" + name, "size": 0, "sha256": sha(b"")})
    seal = {**bound, **identity, "invocation_state": "started", "exit_code": 0,
            "error": "", "predicate": meta["predicate"], "raw_manifest": manifest,
            "manifest_sha256": sha(canonical_go_json(manifest))}
    write_json(directory / "provider.exit", seal, replace=True)
    return seal


def validate_launch(kind: str, directory: Path, meta: dict[str, object],
                    seal: dict[str, object]) -> None:
    write_json(directory / "provider.exit", seal, replace=True)
    if kind == "agy":
        import json
        started = json.loads((directory / "provider.started.json").read_text())
        agy_evidence.validate_provider_exit(
            name="fixture", task=TASK, meta=meta, started=started, provider_exit=seal,
            root_id=ROOT,
            predicate=object_at(meta, "predicate"), expected_verdict="committed")
        return
    method = claude_evidence.validate_task_publication if kind == "claude" else codex_evidence.validate_task_seal
    method(name="fixture", task=TASK, directory=directory, root_id=ROOT,
           label="fixture", pueue_config=Path("/owned/pueue.yml"),
           pueue_version="pueue fixture", pueue=Path("/owned/pueue"), meta=meta)


class InstalledProviderEvidenceTests(unittest.TestCase):
    def test_optional_and_historical_metadata_do_not_pin_the_installation(self):
        for kind in ("claude", "codex", "agy"):
            for pending in (False, True):
                with self.subTest(kind=kind, pending=pending), tempfile.TemporaryDirectory() as tmp:
                    directory = Path(tmp)
                    meta = metadata(directory, pending=pending)
                    validate_meta(kind, directory, meta)
                    if not pending:
                        meta["provider_version"] = "historical diagnostic"
                        object_at(meta, "effective_config", "policy")["runtime_sha256"] = "1" * 64
                        validate_meta(kind, directory, meta)

    def test_metadata_still_requires_identity_permission_and_valid_diagnostics(self):
        damages = (
            lambda m: m.pop("root_id"),
            lambda m: m.update(approval="invalid"),
            lambda m: m.update(provider_executable="relative/provider"),
            lambda m: m.update(provider_version=None),
            lambda m: object_at(m, "effective_config", "policy").update(runtime_sha256=None),
        )
        for kind in ("claude", "codex"):
            for damage in damages:
                with self.subTest(kind=kind, damage=damage), tempfile.TemporaryDirectory() as tmp:
                    directory = Path(tmp)
                    meta = metadata(directory)
                    damage(meta)
                    with self.assertRaises(AcceptanceFailure):
                        validate_meta(kind, directory, meta)

    def test_pending_metadata_requires_capability_and_recorded_environment(self):
        for kind in ("claude", "codex", "agy"):
            for field, value in (("provider_version", "unexpected"),
                                 ("runtime_preflight_sha256", None),
                                 ("runtime_preflight_sha256", "malformed"),
                                 ("environment_recorded", False)):
                with self.subTest(kind=kind, field=field, value=value), tempfile.TemporaryDirectory() as tmp:
                    directory = Path(tmp)
                    meta = metadata(directory, pending=True)
                    meta[field] = value
                    with self.assertRaises(AcceptanceFailure):
                        validate_meta(kind, directory, meta)

    def test_launch_pair_allows_upgrade_but_rejects_malformed_or_changed_diagnostics(self):
        identities = (
            {"provider_executable": "/installed/new-provider"},
            {"provider_executable": "/installed/new-provider", "provider_version": "new",
             "provider_sha256": "2" * 64},
        )
        for kind in ("claude", "codex", "agy"):
            for identity in identities:
                with self.subTest(kind=kind, identity=identity), tempfile.TemporaryDirectory() as tmp:
                    directory = Path(tmp)
                    meta = metadata(directory, pending=True)
                    seal = publication_records(directory, meta, identity)
                    validate_launch(kind, directory, meta, seal)
                    if "provider_version" not in identity:
                        validate_launch(kind, directory, meta, {
                            **seal, "provider_version": "", "provider_sha256": ""})
                    validate_launch(kind, directory, meta, {**seal, "provider_sha256": "3" * 64})
                    for field, value in (("provider_version", None), ("provider_sha256", None),
                                         ("provider_version", "different"),
                                         ("provider_sha256", "not-a-digest"),
                                         ("provider_executable", "relative/provider"),
                                         ("provider_executable", "/different/provider")):
                        with self.subTest(field=field, value=value):
                            changed = {**seal, field: value}
                            with self.assertRaises(AcceptanceFailure):
                                validate_launch(kind, directory, meta, changed)


class InspectionArguments(TypedDict):
    pueue: Path
    config: Path
    base: Path
    config_digest: str
    workspace: Path
    runner: Path
    environment: dict[str, str]
    pueue_version: str


class InstalledProviderSetupTests(unittest.TestCase):
    def test_inspection_definition_matches_go_golden_without_reading_provider(self):
        # Generated by the public Go NewRuntimeInspectionDefinition/Snapshot
        # APIs for /installed/provider, directory /, and environment LANG=C.
        golden = {
            "claude": "a2a9068622cf83e1c2e3eac21d8e6fd5a70e74a10eab8ac3e71c7a8ac266076e",
            "codex": "e4e483fcb4789b3bd9dbad3b714028a5632f6c30fad8efd48413d81d144795d4",
        }
        for kind, module in (("claude", claude_runtime), ("codex", codex_runtime)):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as tmp:
                runner = Path(tmp).resolve(strict=True) / "runner"
                runner.write_bytes(b"owned worker")
                arguments: InspectionArguments = {"pueue": Path("/owned/pueue"), "config": Path("/owned/pueue.yml"),
                             "base": Path("/owned/base"), "config_digest": "a" * 64,
                             "workspace": Path("/"), "runner": runner,
                             "environment": {"LANG": "C"}, "pueue_version": "pueue fixture"}
                with patch.object(module, "supervisor_binding", return_value={"client_executable": "/owned/pueue"}):
                    if kind == "claude":
                        binding = claude_runtime.expected_claude_inspection_binding(
                            executable=Path("/installed/provider"), **arguments)
                    else:
                        binding = codex_runtime.expected_codex_inspection_binding(
                            provider=Path("/installed/provider"), **arguments)
                self.assertEqual(binding["definition_sha256"], golden[kind])
                self.assertNotIn("worker_sha256", binding)
                self.assertEqual(binding["worker_executable"], str(runner))
                self.assertEqual(object_at(binding, "supervisor")["client_executable"], "/owned/pueue")
                self.assertEqual(binding["helper_sha256"], "")

    def test_agy_constructor_never_hashes_provider(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            args = Namespace(delegate="delegate", runner="runner", agy="agy",
                                   pueue="pueue", pueued="pueued")
            prepared = Mock(spec=agy_runner.Prepared, workspace=root / "workspace", scratch=root / "scratch",
                            state=root / "state")
            with patch.object(agy_runner, "reject_tmp"), \
                    patch.object(agy_runner, "reject_runtime_roots"), \
                    patch.object(agy_runner, "resolve_executable", side_effect=lambda value, default, label: root / label), \
                    patch.object(agy_runner, "selected_pueue", side_effect=lambda path: path), \
                    patch.object(agy_runner, "selected_pueued", side_effect=lambda path: path), \
                    patch.object(agy_runner, "selected_tools", return_value=root / "probe"), \
                    patch.object(agy_runner, "require_discovery"), \
                    patch.object(agy_runner, "ProcessBook"), \
                    patch.object(Path, "open", side_effect=AssertionError("provider contents read")):
                owner = agy_runner.NativeRun(args, prepared, root / "output")
            self.assertEqual(owner.provider_version, "")
            self.assertFalse(hasattr(owner, "provider_sha256"))

    def test_claude_and_codex_discovery_never_hash_provider(self):
        for kind, module, owner_type in (
                ("claude", claude_runner, claude_runner.ClaudeAcceptance),
                ("codex", codex_runner, codex_runner.CodexAcceptance)):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                args = Namespace(tools=tmp, delegate="delegate", runner="runner",
                                       pueue="pueue", pueued="pueued", **{kind: kind})
                owner = owner_type.__new__(owner_type)
                with patch.object(module, "resolve_executable", side_effect=lambda value, default, label: root / label), \
                        patch.object(module, "require_discovery"), \
                        patch.object(module, "digest", side_effect=AssertionError("provider hash")):
                    owner._initialize_tools(args)
                self.assertEqual(owner.provider_version, "")
                self.assertFalse(hasattr(owner, "provider_sha256"))

    def test_codex_setup_observes_only_supervisor_versions(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            owner = codex_runner.CodexAcceptance.__new__(codex_runner.CodexAcceptance)
            owner.pueue, owner.pueued, owner.codex = root / "pueue", root / "pueued", root / "codex"
            owner.pueue_base = root
            owner.pueue_config = root / "pueue.yml"
            owner.pueue_config.write_text("config")
            owner.runner, owner.workspace, owner.environment = root / "runner", root, {}
            calls = []
            def direct(name, argv, **kwargs):
                calls.append(argv)
                output = root / name
                output.mkdir()
                (output / "stdout").write_text("pueue fixture")
                return SimpleNamespace(directory=output)
            with patch.object(owner, "direct", side_effect=direct), \
                    patch.object(codex_runner, "expected_codex_inspection_binding", return_value={}), \
                    patch.object(codex_runner, "digest", return_value="a" * 64):
                owner._observe_provider_versions()
            self.assertEqual([argv[0] for argv in calls], [owner.pueue, owner.pueued])

    def test_claude_setup_observes_only_supervisor_versions(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            owner = claude_runner.ClaudeAcceptance.__new__(claude_runner.ClaudeAcceptance)
            owner._pueue_ownership = Mock()
            owner.pueue, owner.pueued, owner.claude = root / "pueue", root / "pueued", root / "claude"
            owner.workspace, owner.runner, owner.environment = root, root / "runner", {}
            owner.state_parent = root
            owner.ops = Mock()
            calls = []
            def direct(name, argv, **kwargs):
                calls.append(argv)
                output = root / name
                output.mkdir()
                (output / "stdout").write_text("pueue fixture")
                return SimpleNamespace(directory=output)
            owner.ops.direct = direct
            owner.processes = Mock()
            owner.processes.start.side_effect = StopIteration("setup observed")
            with patch.object(claude_runner, "private_mkdtemp", return_value=SimpleNamespace(directory=root)), \
                    patch.object(claude_runner, "expected_claude_inspection_binding", return_value={}):
                with self.assertRaisesRegex(StopIteration, "setup observed"):
                    owner.setup()
            self.assertEqual([argv[0] for argv in calls], [owner.pueue, owner.pueued])

    def test_agy_setup_preserves_supervisor_versions_and_probe(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            base = root / "base"
            base.mkdir()
            owner = agy_runner.NativeRun.__new__(agy_runner.NativeRun)
            owner.prepared = Mock(spec=agy_runner.Prepared, state=root, data={})
            owner.output = root
            owner.agy, owner.pueue, owner.pueued = root / "agy", root / "pueue", root / "pueued"
            owner.probe = root / "probe"
            owner.provider_version = ""
            for path in (owner.pueue, owner.pueued):
                path.write_bytes(b"owned supervisor")
            ownership = Mock()
            ownership.adopt_created.return_value.directory = base
            owner.processes = Mock()
            owner.processes.entries = []
            calls = []
            def run(name, argv, *args, **kwargs):
                calls.append(name)
                owner.processes.entries.append(SimpleNamespace(output=lambda: b"pueue fixture"))
            owner.processes.run = run
            owner.processes.start.side_effect = StopIteration("setup observed")
            with patch.object(agy_runner, "PUEUE_PARENT", root), \
                    patch.object(agy_runner, "ensure_private_directory"), \
                    patch.object(agy_runner, "DirectoryOwner", return_value=ownership), \
                    patch.object(agy_runner.tempfile, "mkdtemp", return_value=str(base)):
                with self.assertRaisesRegex(StopIteration, "setup observed"):
                    owner.setup()
            self.assertEqual(calls, ["pueue-version", "pueued-version", "isolation-probe"])
            self.assertEqual(owner.pueue_version, "pueue fixture")
            self.assertEqual(owner.pueued_version, "pueue fixture")


class SupervisorInstalledSelectionTests(unittest.TestCase):
    def test_current_binding_never_reads_executable_bytes_and_keeps_config_identity(self):
        from scripts.acceptance.supervisor.configuration import supervisor_binding
        from scripts.acceptance.supervisor.binding import _validate_supervisor_binding
        with tempfile.TemporaryDirectory(prefix="supervisor-selection-") as tmp:
            root = Path(tmp).resolve()
            executable = root / "pueue"
            executable.write_text("installed fixture\n", encoding="utf-8")
            config = root / "pueue.yml"
            config.write_text("configuration fixture\n", encoding="utf-8")
            config_digest = sha(config.read_bytes())
            with patch.object(Path, "open", side_effect=AssertionError("executable contents read")):
                binding = supervisor_binding(executable, config, root, config_digest, "pueue fixture")
            self.assertNotIn("client_sha256", binding)
            self.assertEqual(binding["client_executable"], str(executable))
            self.assertEqual(binding["config_digest"], config_digest)
            _validate_supervisor_binding(binding, "installed supervisor")
            executable.write_text("compatible installed update\n", encoding="utf-8")
            self.assertEqual(supervisor_binding(executable, config, root, config_digest, "pueue fixture"), binding)
            _validate_supervisor_binding({**binding, "daemon_executable": str(root / "pueued")}, "daemon selection")
            with self.assertRaises(AcceptanceFailure):
                _validate_supervisor_binding({**binding, "config_digest": "invalid"}, "config integrity")

    def test_historical_executable_diagnostics_are_decoded_without_current_hashing(self):
        from scripts.acceptance.supervisor.configuration import supervisor_binding
        from scripts.acceptance.supervisor.binding import _validate_supervisor_binding
        binding = supervisor_binding(Path("/installed/pueue"), Path("/owned/pueue.yml"),
                                     Path("/owned"), "a" * 64, "pueue fixture")
        historical = {**binding, "client_sha256": "b" * 64,
                      "daemon_executable": "/installed/pueued", "daemon_sha256": "c" * 64}
        historical.pop("executable_identity", None)
        _validate_supervisor_binding(historical, "historical supervisor")
        for field in ("client_sha256", "daemon_sha256"):
            with self.subTest(field=field), self.assertRaises(AcceptanceFailure):
                _validate_supervisor_binding({**historical, field: "invalid"}, "historical shape")


    def test_current_supervisor_binding_explicitly_marks_installed_selection(self):
        from scripts.acceptance.supervisor.configuration import supervisor_binding
        binding = supervisor_binding(Path("/installed/pueue"), Path("/owned/pueue.yml"),
                                     Path("/owned"), "a" * 64, "pueue fixture")
        self.assertEqual(binding.get("executable_identity"), "installed-path-v1")

    def test_operational_supervisor_marker_distinguishes_truncated_legacy(self):
        from scripts.acceptance.supervisor.configuration import supervisor_binding
        from scripts.acceptance.supervisor.binding import _validate_supervisor_binding
        current = supervisor_binding(Path("/installed/pueue"), Path("/owned/pueue.yml"),
                                     Path("/owned"), "a" * 64, "pueue fixture")
        truncated = {key: value for key, value in current.items() if key != "executable_identity"}
        with self.assertRaisesRegex(AcceptanceFailure, "historical client_sha256"):
            _validate_supervisor_binding(truncated, "truncated historical supervisor")
        for changed in ({**current, "executable_identity": "future-path-v9"},
                        {**current, "client_sha256": "b" * 64},
                        {**current, "daemon_executable": "/installed/pueued", "daemon_sha256": "c" * 64}):
            with self.subTest(changed=changed), self.assertRaises(AcceptanceFailure):
                _validate_supervisor_binding(changed, "invalid supervisor identity")


if __name__ == "__main__":
    unittest.main()
