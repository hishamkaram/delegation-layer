"""The SSH driver preserves normal configuration and truthful closure evidence."""

from contextlib import redirect_stdout
import base64
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance.processes.contracts import BoundedCommandResult
from scripts.acceptance.remote import cli


class RemoteSSHDriverTests(unittest.TestCase):
    def test_log_oracle_accepts_valid_range_and_rejects_length_or_checksum_corruption(self):
        data = b"finite protocol bytes"
        valid = {"length": len(data), "range_sha256": hashlib.sha256(data).hexdigest()}
        exercise = cli.Exercise(Path("/candidate"), Path("/unused"), Path("/supervisor"), {})
        for field, value in (("length", len(data) + 1), ("range_sha256", "0" * 64)):
            with self.subTest(field=field):
                response = {"logs": {"data_base64": base64.b64encode(data).decode(),
                                     "range": dict(valid)}}
                with patch.object(exercise, "delegate", return_value=response):
                    self.assertEqual(exercise.read_log_range("remote:" + "a" * 32)[0], data)
                    response["logs"]["range"][field] = value
                    with self.assertRaises(cli.AcceptanceError):
                        exercise.read_log_range("remote:" + "a" * 32)

    def test_origin_does_not_override_normal_user_and_system_config(self):
        with tempfile.TemporaryDirectory() as directory:
            exercise = cli.Exercise(Path("/candidate"), Path(directory),
                                    Path("/supervisor/pueue"), {})
            with patch.object(cli, "command", return_value=BoundedCommandResult(0, b"{}")) as command:
                exercise.delegate("devices", "probe", "--device", "remote-fixture")
            argv, environment = command.call_args.args
            self.assertNotIn("--ssh-config", argv)
            self.assertEqual(environment["HOME"], str(Path(directory) / "origin-home"))

    def test_explicit_raw_read_is_not_retained_as_control_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            exercise = cli.Exercise(Path("/candidate"), Path(directory),
                                    Path("/supervisor/pueue"), {})
            with patch.object(cli, "command", return_value=BoundedCommandResult(0, b'{"data_base64":"private"}')):
                response = exercise.delegate("logs", "remote:" + "a" * 32,
                                             "--stream", "stdout", record=False)
            self.assertEqual(response["data_base64"], "private")
            self.assertEqual(list(Path(directory).iterdir()), [])

    def test_prerequisite_failure_writes_nonpassing_receipt_without_launch(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "evidence"
            argv = ["acceptance-remote", "--candidate", directory,
                    "--supervisor", directory, "--output", str(output)]
            with patch.object(cli.sys, "argv", argv), redirect_stdout(io.StringIO()):
                with patch.object(cli, "checked", side_effect=cli.AcceptanceError("image unavailable")):
                    with patch.object(cli.Exercise, "prepare") as prepare:
                        code = cli.main()
            prepare.assert_not_called()
            receipt = json.loads((output / "receipt.json").read_text())
            self.assertEqual(code, 1)
            self.assertEqual(receipt["failure"], "image unavailable")
            self.assertFalse(receipt["canonical_closure"])
            self.assertEqual(receipt["remaining_container"], "")

    def test_missing_closure_or_disposal_cannot_pass(self):
        for closed, removed in ((False, True), (True, False)):
            with self.subTest(closed=closed, removed=removed), tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "evidence"
                exercise = cli.Exercise(Path(directory), output, Path(directory), {},
                                        container="owned" if not removed else "",
                                        closure_verified=closed, container_removed=removed)
                argv = ["acceptance-remote", "--candidate", directory,
                        "--supervisor", directory, "--output", str(output)]
                with patch.object(cli.sys, "argv", argv), redirect_stdout(io.StringIO()):
                    with patch.object(cli, "Exercise", return_value=exercise), patch.object(cli, "checked"):
                        with patch.object(exercise, "prepare"), patch.object(exercise, "exercise"), patch.object(exercise, "cleanup"):
                            self.assertEqual(cli.main(), 1)

    def test_stopped_container_keys_are_erased_only_after_verified_removal(self):
        for removal_fails in (False, True):
            with self.subTest(removal_fails=removal_fails), tempfile.TemporaryDirectory() as directory:
                exercise = cli.Exercise(Path("/candidate"), Path(directory),
                                        Path("/supervisor"), {}, container="owned")
                for name in ("client", "server"):
                    (Path(directory) / name).write_text("owned test key")
                responses = [b"false", cli.AcceptanceError("removal failed") if removal_fails else b""]
                with patch.object(exercise, "capture_control_evidence"), patch.object(exercise, "docker", side_effect=responses):
                    if removal_fails:
                        with self.assertRaisesRegex(cli.AcceptanceError, "removal failed"):
                            exercise.cleanup()
                    else:
                        exercise.cleanup()
                self.assertEqual(exercise.container_removed, not removal_fails)
                for name in ("client", "server"):
                    self.assertEqual((Path(directory) / name).exists(), removal_fails)


if __name__ == "__main__":
    unittest.main()
