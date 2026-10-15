"""Deterministic tests for exact release archive verification and receipts."""

import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import stat
import sys
import tarfile
import tempfile
import threading
import unittest
from argparse import Namespace
from unittest.mock import patch

try:
    from scripts import verify_release_archive as verifier
    from scripts import native_acceptance_evidence as evidence
except ModuleNotFoundError:
    import native_acceptance_evidence as evidence
    import verify_release_archive as verifier


TAG = "v1.2.3"
REVISION = "a" * 40
RELEASE_COMMIT = "c" * 40
SECRET = "release-test-secret-must-not-be-recorded"


class ReleaseArchiveVerificationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="release-archive-test-")
        self.root = Path(self.temporary.name).resolve(strict=True)
        self.runner_temp = self.root / "runner-temp"
        self.runner_temp.mkdir()
        self.assets = self.runner_temp / "delegation-layer-release"
        self.assets.mkdir()
        self.extract = self.assets / "extracted"
        self.receipt_path = self.root / "archive-receipt.json"
        self.platform = verifier.platform_identity()
        self.archive_name = verifier.expected_archive(self.platform, TAG)
        self.write_archive()
        self.install_fake_gh()
        self.environment = {
            "PATH": f"{self.fake_bin}{os.pathsep}{os.environ.get('PATH', os.defpath)}",
            "GH_TOKEN": SECRET,
            "GH_REPO": "owner/repository",
            "HOME": str(self.root),
            "RUNNER_TEMP": str(self.runner_temp),
            "FAKE_DELEGATE_VERSION": "1.2.3",
        }
        self.env_patcher = patch.dict(os.environ, self.environment, clear=False)
        self.env_patcher.start()

    def tearDown(self):
        self.env_patcher.stop()
        self.temporary.cleanup()

    def write_archive(self, *, version="1.2.3", missing=(), extra_secret=False):
        archive = self.assets / self.archive_name
        with tarfile.open(archive, "w:gz") as bundle:
            for name in verifier.BINARIES:
                if name in missing:
                    continue
                contents = self.binary_script(name, version, extra_secret=extra_secret).encode()
                info = tarfile.TarInfo(name)
                info.size = len(contents)
                info.mode = 0o755
                import io

                bundle.addfile(info, io.BytesIO(contents))
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        (self.assets / "checksums.txt").write_text(f"{digest}  {self.archive_name}\n", encoding="ascii")

    def binary_script(self, name, version, *, extra_secret=False):
        if name == "delegate":
            secret_json = f'{{"message":"{SECRET}"}}' if extra_secret else '{"providers":[]}'
            return f"""#!/bin/sh
case "$1" in
  version) printf 'delegate %s\\n' '{version}' ;;
  providers) printf '%s\\n' '{secret_json}' ;;
  capabilities) printf '{{}}\\n' ;;
  supervisor) printf '{{"command":"supervisor","supervisor_control":{{"state":"not_initialized"}}}}\\n' ;;
  *) exit 8 ;;
esac
"""
        if name == "pueue":
            return "#!/bin/sh\nprintf 'pueue 4.0.4\\n'\n"
        if name == "pueued":
            return "#!/bin/sh\nprintf 'pueued 4.0.4\\n'\n"
        return "#!/bin/sh\nexit 0\n"

    def install_fake_gh(self):
        self.fake_bin = self.root / "bin"
        self.fake_bin.mkdir()
        gh = self.fake_bin / "gh"
        gh.write_text(
            "#!/bin/sh\n"
            "case \"$*\" in\n"
            "  *'git/ref/tags/v1.2.3'*) printf 'refs/tags/v1.2.3\\tcommit\\t" + RELEASE_COMMIT + "\\n' ;;\n"
            "  *'git/tags/'*) printf 'commit\\t" + RELEASE_COMMIT + "\\n' ;;\n"
            "esac\n"
            "exit 0\n",
            encoding="utf-8",
        )
        gh.chmod(0o755)

    def verify(self, mode="native-acceptance", *, assets_dir=None, extract_dir=None):
        return verifier.verify(Namespace(
            receipt=str(self.receipt_path),
            assets_dir=str(assets_dir or self.assets),
            extract_dir=str(extract_dir or self.extract),
            mode=mode,
            tag=TAG,
            repo="owner/repository",
            source_revision=REVISION,
        ))

    def load_receipt(self, path=None):
        return json.loads((path or self.receipt_path).read_text(encoding="utf-8"))

    def write_aggregate(self, *, source_revision=REVISION, status="PASS", require_pass=False):
        root = self.root / "native-aggregate-fixture"
        (root / evidence.CASE_ROOT_NAME).mkdir(parents=True)
        (root / evidence.COMPACT_RECEIPT_ROOT_NAME).mkdir()
        matrix = [evidence.MATRIX_HEADER]
        for index, case in enumerate(evidence.EXPECTED_CASES):
            case_directory = evidence.prepare_case_directory(root, case.case_id)
            if status == "PASS" or (status == "FAIL" and index):
                aliases = evidence._expected_task_aliases(case.provider, case.scenario)
                session = "session-" + case.case_id
                tasks = {
                    alias: f"{index + 1:016x}{position + 1:016x}"
                    for position, alias in enumerate(aliases)
                }
                provider_executable = str((self.root / "provider-runtime").resolve())
                provider_runtimes = {
                    task_id: {
                        "executable": provider_executable,
                        "version": f"fixture-{position + 1}",
                        "sha256": hashlib.sha256(
                            f"{case.case_id}:{alias}".encode()
                        ).hexdigest(),
                    }
                    for position, (alias, task_id) in enumerate(tasks.items())
                }
                first_task_id = tasks[aliases[0]]
                first_runtime = provider_runtimes[first_task_id]
                (case_directory / "success.json").write_text(json.dumps({
                    "gate": "acceptance-native", "status": "passed", "provider": case.provider,
                    "mode": case.permission, "scenario": case.scenario,
                    "requested_model": None, "requested_effort": None,
                    "tasks": tasks,
                    "provider_sessions": {alias: session for alias in aliases},
                    "provider_versions": {
                        alias: provider_runtimes[task_id]["version"]
                        for alias, task_id in tasks.items()
                    },
                    "provider_runtime_identities": provider_runtimes,
                    "natural_daemon_shutdown": True,
                }), encoding="utf-8")
                (case_directory / "binding.json").write_text(json.dumps({
                    "provider": case.provider, "mode": case.permission,
                    "scenario": case.scenario,
                    "provider_identity_scope": "first-admitted-task",
                    "provider_task_id": first_task_id,
                    "requested_model": None, "requested_effort": None,
                    "provider_executable": first_runtime["executable"],
                    "provider_version": first_runtime["version"],
                    "provider_sha256": first_runtime["sha256"],
                    "delegate_sha256": hashlib.sha256(
                        (self.extract / "delegate").read_bytes()).hexdigest(),
                    "runner_sha256": hashlib.sha256(
                        (self.extract / "delegate-run").read_bytes()).hexdigest(),
                    "pueue_sha256": hashlib.sha256(
                        (self.extract / "pueue").read_bytes()).hexdigest(),
                    "pueued_sha256": hashlib.sha256(
                        (self.extract / "pueued").read_bytes()).hexdigest(),
                }), encoding="utf-8")
                (case_directory / "cleanup.json").write_text(json.dumps({
                    "status": "removed", "supervisor_shutdown_observed": True,
                    "raw_process_captures": "removed",
                }), encoding="utf-8")
                matrix.append("\t".join((case.case_id, case.provider, case.permission,
                                         case.scenario, "0", "PASS", case.case_output)) + "\n")
            elif status == "BLOCKED":
                (case_directory / "failure.json").write_text(json.dumps({
                    "gate": "acceptance-native", "status": "BLOCKED", "provider": case.provider,
                    "scenario": case.scenario, "requested_model": None,
                    "requested_effort": None,
                }), encoding="utf-8")
                matrix.append("\t".join((case.case_id, case.provider, case.permission,
                                         case.scenario, "2", "BLOCKED", case.case_output)) + "\n")
            else:
                (case_directory / "failure.json").write_text(json.dumps({
                    "gate": "acceptance-native", "status": "failed", "provider": case.provider,
                    "scenario": case.scenario,
                }), encoding="utf-8")
                (case_directory / "raw-provider-output").write_text(SECRET, encoding="utf-8")
                matrix.append("\t".join((case.case_id, case.provider, case.permission,
                                         case.scenario, "1", "FAIL", case.case_output)) + "\n")
        matrix_path = root / evidence.CASE_MATRIX_NAME
        matrix_path.write_text("".join(matrix), encoding="ascii")
        result, _ = evidence.build_aggregate(
            root, matrix_path, root / evidence.AGGREGATE_RECEIPT_NAME,
            tag=TAG, source_revision=source_revision, require_pass=require_pass,
        )
        self.assertEqual(result["status"], status)
        return root / evidence.AGGREGATE_RECEIPT_NAME

    def test_success_binds_exact_archive_binaries_and_default_path(self):
        self.assertEqual(self.verify(), 0)
        terminal_bytes = self.receipt_path.read_bytes()
        self.assertEqual(self.verify(), 0)
        self.assertEqual(self.receipt_path.read_bytes(), terminal_bytes)
        receipt = self.load_receipt()
        self.assertEqual(receipt["verdict"], "pass")
        self.assertEqual(receipt["tag"], TAG)
        self.assertEqual(receipt["package_version"], "1.2.3")
        self.assertEqual(receipt["artifact"]["attestation"], "verified")
        self.assertEqual(receipt["artifact"]["archive_sha256"], hashlib.sha256(
            (self.assets / self.archive_name).read_bytes()
        ).hexdigest())
        self.assertEqual(len(receipt["artifact"]["checksums_sha256"]), 64)
        self.assertEqual(receipt["platform"]["os"], platform.system() if platform.system() == "Linux" else "Darwin")
        self.assertTrue(receipt["platform"]["kernel"])
        self.assertIn(receipt["platform"]["arch"], ("amd64", "arm64"))
        self.assertEqual(set(receipt["binaries"]), set(verifier.BINARIES))
        for name, binary in receipt["binaries"].items():
            self.assertEqual(Path(binary["path"]).resolve(), (self.extract / name).resolve())
            self.assertEqual(binary["sha256"], hashlib.sha256((self.extract / name).read_bytes()).hexdigest())
        self.assertEqual(receipt["delegate_build_identity"]["version"], "1.2.3")
        self.assertEqual(receipt["default_path_observation"]["observed_state"], "not_initialized")
        self.assertEqual(receipt["default_path_observation"]["explicit_path_overrides"], [])
        self.assertEqual(receipt["default_path_observation"]["path_source"], "PATH")
        self.assertEqual(receipt["default_path_observation"]["command_name"], "delegate-default-supervisor-status")
        self.assertTrue(all(row["exit_code"] == 0 for row in receipt["commands"]))
        self.assertTrue(receipt["unresolved_limits"])
        self.assertEqual(receipt["cleanup"]["status"], "retained-for-native-acceptance")
        home = receipt["default_path_observation"]["isolated_home"]
        self.assertIn(home, receipt["cleanup"]["removed_paths"])
        self.assertFalse(Path(home).exists())
        for identity in receipt["cleanup"]["directory_identities"].values():
            self.assertIn("parent", identity)
            self.assertIn("parent_device", identity)
            self.assertIn("parent_inode", identity)
        self.assertEqual(receipt["source_revision"], REVISION)
        self.assertEqual(receipt["harness_revision"], REVISION)
        self.assertEqual(receipt["release_revision"], RELEASE_COMMIT)
        self.assertEqual(receipt["release_identity"], {
            "repository": "owner/repository", "tag": TAG,
            "ref": f"refs/tags/{TAG}", "commit_sha": RELEASE_COMMIT,
        })
        ref_command = next(row for row in receipt["commands"] if row["name"] == "github-release-ref")
        attestation = next(row for row in receipt["commands"]
                           if row["name"] == "github-attestation-verify")
        self.assertIn("--source-ref", attestation["argv"])
        self.assertIn(f"refs/tags/{TAG}", attestation["argv"])
        self.assertIn("--source-digest", attestation["argv"])
        self.assertEqual(ref_command["exit_code"], 0)
        self.assertEqual(len(receipt["source_harness"]["sha256"]), 64)
        fingerprint_paths = {item["path"] for item in receipt["source_harness"]["files"]}
        self.assertIn("scripts/acceptance_native.py", fingerprint_paths)
        self.assertIn("scripts/acceptance_provider_common.py", fingerprint_paths)

    def test_checksum_mismatch_finalizes_receipt_and_retains_assets(self):
        (self.assets / "checksums.txt").write_text(f"{'0' * 64}  {self.archive_name}\n", encoding="ascii")
        self.assertEqual(self.verify(), 1)
        receipt = self.load_receipt()
        self.assertEqual(receipt["failure"]["code"], "archive_checksum_mismatch")
        self.assertEqual(receipt["verdict"], "fail")
        self.assertTrue(self.assets.exists())
        self.assertFalse(list(self.root.glob(".*.tmp")))
        terminal_bytes = self.receipt_path.read_bytes()
        digest = hashlib.sha256((self.assets / self.archive_name).read_bytes()).hexdigest()
        (self.assets / "checksums.txt").write_text(
            f"{digest}  {self.archive_name}\n", encoding="ascii",
        )
        self.assertEqual(self.verify(), 1)
        self.assertEqual(self.receipt_path.read_bytes(), terminal_bytes)

    def test_archive_path_mismatch_is_rejected(self):
        wrong_name = "delegation-layer_1.2.3_Wrong_amd64.tar.gz"
        (self.assets / self.archive_name).rename(self.assets / wrong_name)
        checksum = self.assets / "checksums.txt"
        digest = hashlib.sha256((self.assets / wrong_name).read_bytes()).hexdigest()
        checksum.write_text(f"{digest}  {wrong_name}\n", encoding="ascii")
        self.assertEqual(self.verify(), 1)
        self.assertEqual(self.load_receipt()["failure"]["code"], "archive_path_mismatch")

    def test_delegate_version_mismatch_is_rejected(self):
        self.write_archive(version="9.8.7")
        self.assertEqual(self.verify(), 1)
        receipt = self.load_receipt()
        self.assertEqual(receipt["failure"]["code"], "delegate_version_mismatch")
        self.assertEqual(receipt["delegate_build_identity"]["version"], "9.8.7")

    def test_missing_binary_is_rejected_with_terminal_receipt(self):
        self.write_archive(missing=("delegate-run",))
        self.assertEqual(self.verify(), 1)
        receipt = self.load_receipt()
        self.assertEqual(receipt["failure"]["code"], "release_binary_missing")
        self.assertEqual(receipt["failure"]["stage"], "extract-archive")
        self.assertEqual(receipt["verdict"], "fail")

    def test_cli_command_failure_exit_code_is_recorded(self):
        script = self.binary_script("delegate", "1.2.3").replace(
            "providers) printf", "providers) exit 17 ;;\n  providers_old) printf"
        )
        self.replace_delegate_script(script)
        self.assertEqual(self.verify(), 1)
        receipt = self.load_receipt()
        self.assertEqual(receipt["failure"]["code"], "command_failed")
        command = next(row for row in receipt["commands"] if row["name"] == "delegate-providers")
        self.assertEqual(command["exit_code"], 17)

    def test_successful_archive_smoke_cleans_owned_assets_and_extraction(self):
        source = self.root / "download-source"
        source.mkdir()
        shutil.copy2(self.assets / self.archive_name, source / self.archive_name)
        shutil.copy2(self.assets / "checksums.txt", source / "checksums.txt")
        smoke_runner = self.root / "smoke-runner"
        smoke_runner.mkdir()
        destination = smoke_runner / "delegation-layer-release"
        extract_dir = destination / "extracted"
        self.receipt_path = self.root / "smoke-receipt.json"
        gh = self.fake_bin / "gh"
        gh.write_text(
            "#!/bin/sh\n"
            "case \"$*\" in\n"
            "  *'git/ref/tags/v1.2.3'*) printf 'refs/tags/v1.2.3\\tcommit\\t" + RELEASE_COMMIT + "\\n' ;;\n"
            "  *'git/tags/'*) printf 'commit\\t" + RELEASE_COMMIT + "\\n' ;;\n"
            "esac\n"
            "if [ \"$1\" = release ] && [ \"$2\" = download ]; then\n"
            f"  mkdir -p '{destination}'\n"
            f"  cp '{source / self.archive_name}' '{destination / self.archive_name}'\n"
            f"  cp '{source / 'checksums.txt'}' '{destination / 'checksums.txt'}'\n"
            "fi\nexit 0\n",
            encoding="utf-8",
        )
        gh.chmod(0o755)
        with patch.dict(os.environ, {"RUNNER_TEMP": str(smoke_runner)}):
            result = self.verify(mode="archive-smoke", assets_dir=destination, extract_dir=extract_dir)
        self.assertEqual(result, 0)
        receipt = self.load_receipt()
        self.assertEqual(receipt["cleanup"]["status"], "complete")
        self.assertFalse(destination.exists())
        self.assertEqual(receipt["verdict"], "pass")
        terminal = self.receipt_path.read_bytes()
        with patch.dict(os.environ, {"RUNNER_TEMP": str(smoke_runner)}):
            replay = self.verify(
                mode="archive-smoke", assets_dir=destination, extract_dir=extract_dir,
            )
        self.assertEqual(replay, 0)
        self.assertEqual(self.receipt_path.read_bytes(), terminal)

    def test_receipt_redacts_command_output_and_environment_secrets(self):
        self.write_archive(extra_secret=True)
        self.assertEqual(self.verify(), 0)
        raw = self.receipt_path.read_text(encoding="utf-8")
        self.assertNotIn(SECRET, raw)
        self.assertNotIn("GH_TOKEN", raw)
        self.assertNotIn("message", raw)

    def test_command_stops_at_output_bound_during_execution(self):
        receipt = {"commands": []}
        with self.assertRaises(verifier.VerificationError) as raised:
            verifier.command(
                receipt, "oversized-output", [sys.executable, "-c",
                f"import sys; sys.stdout.buffer.write(b'x' * {verifier.MAX_OUTPUT + 1})"],
                os.environ.copy(), capture=True, timeout=5,
            )
        self.assertEqual(raised.exception.code, "command_output_exceeded")
        self.assertEqual(len(receipt["commands"]), 1)
        self.assertLessEqual(len(receipt["commands"][0]), 4)

    def test_release_paths_reject_parent_components_before_resolution(self):
        with self.assertRaises(verifier.VerificationError) as raised:
            verifier.release_paths(Namespace(
                assets_dir=str(self.runner_temp / "delegation-layer-release" / ".." / "other"),
                extract_dir=str(self.extract),
            ), self.receipt_path)
        self.assertEqual(raised.exception.code, "unsafe_release_paths")

    def test_cleanup_authorization_is_bound_to_the_exact_target_identity(self):
        first = self.root / "cleanup-target-first"
        second = self.root / "cleanup-target-second"
        first.mkdir()
        second.mkdir()
        first_identity = evidence._directory_identity(first)
        second_identity = evidence._directory_identity(second)
        receipt = verifier.new_receipt("archive-smoke", TAG, REVISION)
        authorization = verifier._release_cleanup_authorization(
            receipt, "pass", first_identity,
        )
        receipt["proof"] = {
            "state": "committed",
            "authorization_sha256": authorization,
            "final_verdict": "pass",
        }
        receipt["cleanup"].update({
            "status": "prepared",
            "transaction": {
                "journal_path": str(self.root / "cleanup.json"),
                "authorization_sha256": authorization,
                "target_identity": second_identity,
            },
        })

        with self.assertRaisesRegex(
                verifier.VerificationError, "release_cleanup_authorization_changed"):
            verifier._release_cleanup_transaction(receipt)
        self.assertTrue(first.is_dir())
        self.assertTrue(second.is_dir())

    def test_native_finalizer_binds_archive_receipt_and_cleans_after_success(self):
        self.assertEqual(self.verify(), 0)
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        aggregate_receipt = self.write_aggregate(source_revision=REVISION)
        final_receipt_path = self.root / "native-receipt.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path),
            archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt),
            assets_dir=str(self.assets),
            extract_dir=str(self.extract),
            acceptance_outcome="success",
            exit_code_file=str(exit_file),
            tag=TAG,
            source_revision=REVISION,
        ))
        self.assertEqual(code, 0)
        receipt = json.loads(final_receipt_path.read_text(encoding="utf-8"))
        self.assertEqual(receipt["verdict"], "pass")
        self.assertEqual(receipt["acceptance"]["exit_code"], 0)
        archive_receipt = self.load_receipt()
        self.assertEqual(receipt["binaries"], archive_receipt["binaries"])
        self.assertEqual(receipt["archive_receipt_sha256"], hashlib.sha256(self.receipt_path.read_bytes()).hexdigest())
        self.assertEqual(receipt["aggregate_receipt_sha256"],
                         hashlib.sha256(aggregate_receipt.read_bytes()).hexdigest())
        self.assertEqual(len(receipt["native_acceptance"]["case_receipt_hashes"]), len(evidence.EXPECTED_CASES))
        self.assertTrue(all(len(item["receipt_sha256"]) == 64
                            for item in receipt["native_acceptance"]["case_receipt_hashes"]))
        self.assertEqual(receipt["cleanup"]["status"], "complete")
        self.assertFalse(self.assets.exists())
        staged = self.root / "staged-native-evidence"
        manifest = evidence.stage_evidence(
            aggregate_receipt,
            staged,
            archive_receipt=self.receipt_path,
            native_receipt=final_receipt_path,
            expected_tag=TAG,
            expected_source_revision=REVISION,
        )
        self.assertEqual(len(manifest["release_cleanup_journals"]), 1)
        self.assertTrue(
            (staged / manifest["release_cleanup_journals"][0]["path"]).is_file(),
        )

    def test_native_finalizer_binds_every_case_digest_and_rejects_tampering(self):
        self.assertEqual(self.verify(), 0)
        aggregate_receipt = self.write_aggregate()
        aggregate = json.loads(aggregate_receipt.read_text(encoding="utf-8"))
        compact = Path(aggregate["cases"][0]["receipt_path"])
        compact.write_text(compact.read_text(encoding="utf-8") + "tampered\n", encoding="utf-8")
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        final_receipt_path = self.root / "native-tampered-case.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), acceptance_outcome="success",
            exit_code_file=str(exit_file), tag=TAG, source_revision=REVISION,
        ))
        self.assertEqual(code, 1)
        receipt = json.loads(final_receipt_path.read_text(encoding="utf-8"))
        self.assertEqual(receipt["failure"]["code"], "aggregate_case_digest_mismatch")
        self.assertTrue(self.assets.exists())

    def test_native_finalizer_rejects_case_binaries_not_bound_to_archive(self):
        self.assertEqual(self.verify(), 0)
        aggregate_receipt = self.write_aggregate()
        aggregate = json.loads(aggregate_receipt.read_text(encoding="utf-8"))
        first = aggregate["cases"][0]
        compact_path = Path(first["receipt_path"])
        compact = json.loads(compact_path.read_text(encoding="utf-8"))
        compact["source_proof"]["binary_identities"]["sha256"]["delegate_sha256"] = "f" * 64
        compact_bytes = (json.dumps(
            compact, sort_keys=True, separators=(",", ":"), allow_nan=False,
        ) + "\n").encode()
        compact_path.write_bytes(compact_bytes)
        first["receipt_sha256"] = hashlib.sha256(compact_bytes).hexdigest()
        first["binary_identities"] = compact["source_proof"]["binary_identities"]
        aggregate_receipt.write_text(
            json.dumps(aggregate, sort_keys=True, separators=(",", ":")) + "\n",
            encoding="utf-8",
        )
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        final_receipt = self.root / "native-case-binary-mismatch.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), acceptance_outcome="success",
            exit_code_file=str(exit_file), tag=TAG, source_revision=REVISION,
        ))
        self.assertEqual(code, 1)
        self.assertEqual(self.load_receipt(final_receipt)["failure"]["code"],
                         "aggregate_case_cleanup_journal_invalid")
        self.assertTrue(self.assets.exists())

    def test_native_finalizer_replays_after_terminal_write_interruption(self):
        self.assertEqual(self.verify(), 0)
        aggregate_receipt = self.write_aggregate()
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        final_receipt = self.root / "native-cleanup-replay.json"
        args = Namespace(
            receipt=str(final_receipt), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), acceptance_outcome="success",
            exit_code_file=str(exit_file), tag=TAG, source_revision=REVISION,
        )
        original_write = verifier.write_receipt
        interrupted = False

        def interrupt_final_write(path, receipt):
            nonlocal interrupted
            completion = receipt.get("completion")
            if (not interrupted and isinstance(completion, dict)
                    and completion.get("state") == "complete"):
                interrupted = True
                raise verifier.VerificationError("forced_terminal_write_failure", "write-receipt")
            return original_write(path, receipt)

        with patch.object(verifier, "write_receipt", side_effect=interrupt_final_write):
            with self.assertRaisesRegex(verifier.VerificationError,
                                        "forced_terminal_write_failure"):
                verifier.finalize_native(args)
        prepared = self.load_receipt(final_receipt)
        self.assertEqual(prepared["cleanup"]["status"], "prepared")
        self.assertEqual(prepared["completion"]["state"], "prepared")
        self.assertFalse(self.assets.exists())
        self.assertEqual(verifier.finalize_native(args), 0)
        terminal = self.load_receipt(final_receipt)
        self.assertEqual((terminal["verdict"], terminal["cleanup"]["status"],
                          terminal["completion"]["state"]),
                         ("pass", "complete", "complete"))

    def test_release_receipt_serializes_replay_through_terminal_publication(self):
        original_write = verifier.write_receipt
        first_entered = threading.Event()
        release_first = threading.Event()
        second_started = threading.Event()
        second_done = threading.Event()
        results = []
        errors = []
        writes = []

        def pause_first_writer(path, receipt):
            name = threading.current_thread().name
            writes.append(name)
            if name == "release-first" and not first_entered.is_set():
                first_entered.set()
                if not release_first.wait(5):
                    raise RuntimeError("release receipt lock fixture timed out")
            return original_write(path, receipt)

        def run(name, started=None, done=None):
            try:
                if started is not None:
                    started.set()
                results.append((name, self.verify()))
            except BaseException as error:  # Preserve thread failures for the assertion.
                errors.append(error)
            finally:
                if done is not None:
                    done.set()

        with patch.object(verifier, "write_receipt", side_effect=pause_first_writer):
            first = threading.Thread(target=run, args=("first",), name="release-first")
            second = threading.Thread(
                target=run, args=("second", second_started, second_done), name="release-second",
            )
            first.start()
            self.assertTrue(first_entered.wait(5))
            second.start()
            self.assertTrue(second_started.wait(5))
            self.assertFalse(second_done.wait(0.2))
            release_first.set()
            first.join(10)
            second.join(10)

        self.assertEqual(errors, [])
        self.assertEqual(sorted(results), [("first", 0), ("second", 0)])
        self.assertNotIn("release-second", writes)
        self.assertEqual((self.load_receipt()["stage"], self.load_receipt()["verdict"]),
                         ("complete", "pass"))

    def test_native_finalizer_rejects_a_packaged_archive_receipt_replay(self):
        self.assertEqual(self.verify(), 0)
        terminal_bytes = self.receipt_path.read_bytes()
        args = Namespace(
            receipt=str(self.receipt_path), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(self.root / "missing-aggregate.json"),
            assets_dir=str(self.assets), extract_dir=str(self.extract),
            acceptance_outcome="failure", exit_code_file=None, tag=TAG,
            source_revision=REVISION,
        )

        with self.assertRaisesRegex(verifier.VerificationError,
                                    "release_terminal_receipt_mismatch"):
            verifier.finalize_native(args)
        self.assertEqual(self.receipt_path.read_bytes(), terminal_bytes)
        self.assertTrue(self.assets.is_dir())

    def test_successful_cleanup_recovery_clears_the_active_failure(self):
        self.assertEqual(self.verify(), 0)
        aggregate_receipt = self.write_aggregate()
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        final_receipt = self.root / "native-cleanup-recovery.json"
        args = Namespace(
            receipt=str(final_receipt), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), acceptance_outcome="success",
            exit_code_file=str(exit_file), tag=TAG, source_revision=REVISION,
        )

        cleanup_module = sys.modules[verifier.resume_directory_cleanup.__module__]
        with patch.object(cleanup_module, "_cleanup_empty",
                          side_effect=OSError("simulated cleanup interruption")):
            self.assertEqual(verifier.finalize_native(args), 1)
        self.assertEqual(self.load_receipt(final_receipt)["failure"]["code"],
                         "release_cleanup_failed")
        self.assertEqual(verifier.finalize_native(args), 0)
        recovered = self.load_receipt(final_receipt)
        self.assertIsNone(recovered["failure"])
        self.assertEqual((recovered["verdict"], recovered["completion"]["state"]),
                         ("pass", "complete"))

    def test_cleanup_never_starts_before_prepared_receipt_is_durable(self):
        self.assertEqual(self.verify(), 0)
        aggregate_receipt = self.write_aggregate()
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        final_receipt = self.root / "native-prepare-failure.json"
        args = Namespace(
            receipt=str(final_receipt), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), acceptance_outcome="success",
            exit_code_file=str(exit_file), tag=TAG, source_revision=REVISION,
        )
        original_write = verifier.write_receipt
        calls = 0

        def interrupt_prepare(path, receipt):
            nonlocal calls
            calls += 1
            if calls == 2:
                raise verifier.VerificationError("forced_prepare_write_failure", "write-receipt")
            return original_write(path, receipt)

        with patch.object(verifier, "write_receipt", side_effect=interrupt_prepare):
            with self.assertRaisesRegex(verifier.VerificationError, "forced_prepare_write_failure"):
                verifier.finalize_native(args)
        self.assertTrue(self.assets.is_dir())
        self.assertTrue(self.extract.is_dir())

    def test_missing_cleanup_target_never_becomes_terminal_success(self):
        self.assertEqual(self.verify(), 0)
        aggregate_receipt = self.write_aggregate()
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        final_receipt = self.root / "native-missing-cleanup-target.json"
        args = Namespace(
            receipt=str(final_receipt), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), acceptance_outcome="success",
            exit_code_file=str(exit_file), tag=TAG, source_revision=REVISION,
        )
        original_write = verifier.write_receipt
        removed = False

        def remove_after_prepare(path, receipt):
            nonlocal removed
            result = original_write(path, receipt)
            completion = receipt.get("completion")
            if (not removed and isinstance(completion, dict)
                    and completion.get("state") == "prepared"):
                removed = True
                shutil.rmtree(self.assets)
            return result

        with patch.object(verifier, "write_receipt", side_effect=remove_after_prepare):
            self.assertEqual(verifier.finalize_native(args), 1)
        prepared = self.load_receipt(final_receipt)
        self.assertEqual(prepared["cleanup"]["status"], "incomplete")
        self.assertEqual(prepared["completion"]["state"], "prepared")
        self.assertNotEqual(prepared["verdict"], "pass")
        with self.assertRaisesRegex(verifier.VerificationError, "release_cleanup_failed"):
            verifier.finalize_native(args)
        self.assertEqual(self.load_receipt(final_receipt)["completion"]["state"], "prepared")

    def test_native_finalizer_keeps_blocked_claims_case_scoped(self):
        self.assertEqual(self.verify(), 0)
        aggregate_receipt = self.write_aggregate(status="BLOCKED", require_pass=True)
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("2\n", encoding="ascii")
        final_receipt_path = self.root / "native-blocked.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), acceptance_outcome="failure",
            exit_code_file=str(exit_file), tag=TAG, source_revision=REVISION,
        ))
        self.assertEqual(code, 1)
        receipt = json.loads(final_receipt_path.read_text(encoding="utf-8"))
        self.assertEqual(receipt["failure"]["code"], "native_acceptance_pass_required")
        self.assertEqual(receipt["native_acceptance"]["aggregate_status"], "BLOCKED")
        self.assertEqual(receipt["native_acceptance"]["claims_scope"],
                         "provider-permission-scenario-case-results-only")
        self.assertEqual(len(receipt["native_acceptance"]["case_receipt_hashes"]), len(evidence.EXPECTED_CASES))
        self.assertTrue(all(item["status"] == "BLOCKED"
                            for item in receipt["native_acceptance"]["case_receipt_hashes"]))
        self.assertTrue(self.assets.exists())

    def test_native_finalizer_all_blocked_is_neutral_without_pass_policy(self):
        self.assertEqual(self.verify(), 0)
        aggregate_receipt = self.write_aggregate(status="BLOCKED", require_pass=False)
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        final_receipt_path = self.root / "native-blocked-neutral.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), acceptance_outcome="success",
            exit_code_file=str(exit_file), tag=TAG, source_revision=REVISION,
        ))
        self.assertEqual(code, 0)
        receipt = json.loads(final_receipt_path.read_text(encoding="utf-8"))
        self.assertEqual(receipt["verdict"], "blocked")
        self.assertEqual(receipt["native_acceptance"]["aggregate_status"], "BLOCKED")
        self.assertFalse(self.assets.exists())

    def test_native_finalizer_rejects_changed_binary_hash(self):
        self.assertEqual(self.verify(), 0)
        (self.extract / "delegate-run").write_bytes(b"replaced release binary")
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        aggregate_receipt = self.write_aggregate()
        final_receipt_path = self.root / "native-hash-mismatch.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt),
            assets_dir=str(self.assets), extract_dir=str(self.extract),
            acceptance_outcome="success", exit_code_file=str(exit_file),
            tag=TAG, source_revision=REVISION,
        ))
        self.assertEqual(code, 1)
        receipt = json.loads(final_receipt_path.read_text(encoding="utf-8"))
        self.assertEqual(receipt["failure"]["code"], "release_binary_identity_changed")
        self.assertTrue(self.extract.exists())

    def test_native_finalizer_rejects_binary_path_replacement(self):
        self.assertEqual(self.verify(), 0)
        outside = self.root / "outside-binary"
        outside.write_bytes((self.extract / "pueue").read_bytes())
        (self.extract / "pueue").unlink()
        (self.extract / "pueue").symlink_to(outside)
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("0\n", encoding="ascii")
        aggregate_receipt = self.write_aggregate()
        final_receipt_path = self.root / "native-path-mismatch.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt),
            assets_dir=str(self.assets), extract_dir=str(self.extract),
            acceptance_outcome="success", exit_code_file=str(exit_file),
            tag=TAG, source_revision=REVISION,
        ))
        self.assertEqual(code, 1)
        receipt = json.loads(final_receipt_path.read_text(encoding="utf-8"))
        self.assertEqual(receipt["failure"]["code"], "release_binary_invalid")
        self.assertTrue(self.extract.exists())

    def test_native_finalizer_rejects_changed_archive_checksum_and_writes_receipt(self):
        self.assertEqual(self.verify(), 0)
        (self.assets / "checksums.txt").write_text("changed\n", encoding="ascii")
        aggregate_receipt = self.write_aggregate()
        final_receipt_path = self.root / "native-checksum-mismatch.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt),
            assets_dir=str(self.assets), extract_dir=str(self.extract),
            acceptance_outcome="success", exit_code_file=None, tag=TAG,
            source_revision=REVISION,
        ))
        self.assertEqual(code, 1)
        receipt = json.loads(final_receipt_path.read_text(encoding="utf-8"))
        self.assertEqual(receipt["failure"]["code"], "archive_receipt_invalid")
        self.assertTrue(self.assets.exists())

    def test_native_finalizer_missing_archive_receipt_is_inconclusive(self):
        self.assertEqual(self.verify(), 0)
        final_receipt_path = self.root / "native-missing-archive-receipt.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path), archive_receipt=str(self.root / "missing.json"),
            assets_dir=str(self.assets), extract_dir=str(self.extract),
            acceptance_outcome="skipped", exit_code_file=None, tag=TAG,
            source_revision=REVISION,
        ))
        self.assertEqual(code, 1)
        receipt = json.loads(final_receipt_path.read_text(encoding="utf-8"))
        self.assertEqual(receipt["failure"]["code"], "archive_receipt_missing_or_invalid")
        self.assertTrue(self.assets.exists())
        self.assertIn(str(self.assets), receipt["cleanup"]["retained_paths"])

    def test_native_finalizer_rejects_release_identity_tampering(self):
        self.assertEqual(self.verify(), 0)
        aggregate_receipt = self.write_aggregate()
        archive = self.load_receipt()
        archive["release_identity"]["ref"] = "refs/heads/main"
        self.receipt_path.write_text(json.dumps(archive), encoding="utf-8")
        final_receipt_path = self.root / "native-release-identity-tampered.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path), archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt), assets_dir=str(self.assets),
            extract_dir=str(self.extract), acceptance_outcome="success",
            exit_code_file=None, tag=TAG, source_revision=REVISION,
        ))
        self.assertEqual(code, 1)
        receipt = self.load_receipt(final_receipt_path)
        self.assertEqual(receipt["failure"]["code"], "release_identity_unbound")
        self.assertTrue(self.assets.exists())

    def test_native_failure_retains_binary_and_emits_receipt(self):
        self.assertEqual(self.verify(), 0)
        exit_file = self.root / "acceptance.exit"
        exit_file.write_text("1\n", encoding="ascii")
        aggregate_receipt = self.write_aggregate(status="FAIL")
        final_receipt_path = self.root / "native-failed.json"
        code = verifier.finalize_native(Namespace(
            receipt=str(final_receipt_path),
            archive_receipt=str(self.receipt_path),
            aggregate_receipt=str(aggregate_receipt),
            assets_dir=str(self.assets),
            extract_dir=str(self.extract),
            acceptance_outcome="failure",
            exit_code_file=str(exit_file),
            tag=TAG,
            source_revision=REVISION,
        ))
        self.assertEqual(code, 1)
        receipt = json.loads(final_receipt_path.read_text(encoding="utf-8"))
        self.assertEqual(receipt["failure"]["code"], "native_acceptance_failed")
        self.assertTrue(self.extract.exists())
        self.assertIn(str(self.assets), receipt["cleanup"]["retained_paths"])

    def replace_delegate_script(self, script):
        temporary_extract = self.root / "replacement"
        temporary_extract.mkdir()
        archive = self.assets / self.archive_name
        with tarfile.open(archive, "w:gz") as bundle:
            for name in verifier.BINARIES:
                contents = script if name == "delegate" else self.binary_script(name, "1.2.3")
                path = temporary_extract / name
                path.write_text(contents, encoding="utf-8")
                path.chmod(stat.S_IRUSR | stat.S_IWUSR | stat.S_IXUSR)
                bundle.add(path, arcname=name)
        digest = hashlib.sha256(archive.read_bytes()).hexdigest()
        (self.assets / "checksums.txt").write_text(f"{digest}  {self.archive_name}\n", encoding="ascii")


if __name__ == "__main__":
    unittest.main()
