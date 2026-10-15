"""Byte-preserving receipts for finite private-CLI capture fixtures."""

import hashlib
import io
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import acceptance_private_cli as private_cli
from acceptance_supervisor_common import DirectoryOwner


class PrivateCaptureTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-capture-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.capture_owner = DirectoryOwner()
        self.capture_root = self.capture_owner.create(
            "private capture root", self.root / "capture-root", self.root,
        )
        self.addCleanup(self.capture_owner.rollback)
        self.receipt = self.root / "invocation.json"
        self.environment = os.environ.copy()

    @staticmethod
    def child_args(stdout, stderr, code=0):
        return ["-c", (
            "import os, sys\n"
            f"os.write(1, {stdout!r})\n"
            f"os.write(2, {stderr!r})\n"
            f"sys.exit({code})\n"
        )]

    def read_receipt(self):
        return json.loads(self.receipt.read_text(encoding="utf-8"))

    def assert_error_receipt(self, args, error, stdout, stderr, code):
        receipt = self.read_receipt()
        self.assertEqual(receipt["arguments"], args)
        self.assertEqual(receipt["exit_code"], code)
        self.assertEqual(receipt["invocation_error"], str(error))
        self.assertEqual(receipt["failure_class"], "invocation_error")
        for name, raw in (("stdout", stdout), ("stderr", stderr)):
            self.assertEqual(receipt[f"{name}_bytes"], len(raw))
            self.assertEqual(receipt[f"{name}_sha256"], hashlib.sha256(raw).hexdigest())
            self.assertEqual(receipt[f"{name}_tail"], private_cli.output_tail(raw))
            self.assertNotIn(name, receipt)
            self.assertNotIn(f"raw_{name}_base64", receipt)

    def test_capture_returns_exact_bytes_and_exit_code(self):
        stdout = b'{"value":"\xff"}\n\x00'
        stderr = b"diagnostic\x80\r\n"
        command = [sys.executable, *self.child_args(stdout, stderr, code=7)]
        result = private_cli.run_captured_command(
            command, self.environment, timeout=3, capture_root=self.capture_root,
        )
        self.assertEqual(result.args, command)
        self.assertEqual(result.returncode, 7)
        self.assertIsInstance(result.stdout, bytes)
        self.assertIsInstance(result.stderr, bytes)
        self.assertEqual(result.stdout, stdout)
        self.assertEqual(result.stderr, stderr)
        self.assertEqual(
            len(tuple(self.capture_root.directory.iterdir())), 2,
            "capture streams remain owned until the acceptance root is cleaned up",
        )

    def test_invalid_utf8_in_either_stream_saves_both_raw_streams(self):
        valid_stdout = '{"message":"café"}\n'.encode("utf-8")
        valid_stderr = "warning: café\n".encode("utf-8")
        for name, stdout, stderr, code in (
            ("stdout", b'{"message":"\xff"}\n', valid_stderr, 0),
            ("stderr", valid_stdout, b"warning: \xfe\n", 7),
            ("both", b'{"message":"\xff"}\n', b"warning: \xfe\n", 2),
        ):
            with self.subTest(stream=name):
                args = self.child_args(stdout, stderr, code)
                with self.assertRaises(UnicodeDecodeError) as raised:
                    private_cli.run_json(
                        sys.executable, self.environment, args, timeout=3,
                        receipt=self.receipt, capture_root=self.capture_root,
                    )
                self.assertEqual(
                    raised.exception.object, stderr if name == "stderr" else stdout,
                )
                self.assert_error_receipt(args, raised.exception, stdout, stderr, code)

    def test_timeout_preserves_partial_utf8_and_timeout_exception(self):
        stdout = b'{"message":"unfinished \xe2\x82'
        stderr = b"unfinished diagnostic \xf0\x9f"
        ready_read, ready_write = os.pipe()
        self.addCleanup(os.close, ready_read)
        self.addCleanup(os.close, ready_write)
        args = ["-c", (
            "import os, time\n"
            f"os.write(1, {stdout!r})\n"
            f"os.write(2, {stderr!r})\n"
            f"os.write({ready_write}, b'R')\n"
            f"os.close({ready_write})\n"
            "time.sleep(30)\n"
        )]
        real_popen = subprocess.Popen
        children = []

        def start_after_output(*command, **options):
            process = real_popen(*command, **options, pass_fds=(ready_write,))
            children.append(process)
            self.addCleanup(self.reap_child, process)
            # Start the fault timeout only after both output writes complete.
            # The generous setup bound is independent of the short wait fault.
            readable, _, _ = select.select([ready_read], [], [], 10)
            self.assertEqual(readable, [ready_read], "child did not emit its output")
            self.assertEqual(os.read(ready_read, 1), b"R")
            return process

        timeout = 0.25
        with patch.object(private_cli.subprocess, "Popen", side_effect=start_after_output):
            with self.assertRaises(subprocess.TimeoutExpired) as raised:
                private_cli.run_json(
                    sys.executable, self.environment, args, timeout=timeout,
                    receipt=self.receipt, capture_root=self.capture_root,
                )
        error = raised.exception
        self.assertEqual(error.cmd, [sys.executable, *args])
        self.assertEqual(error.timeout, timeout)
        self.assertEqual(error.output, stdout)
        self.assertEqual(error.stderr, stderr)
        self.assert_error_receipt(args, error, stdout, stderr, None)
        self.assertEqual(len(children), 1)
        self.assertIsNotNone(children[0].returncode, "timed-out child was not reaped")

    @staticmethod
    def reap_child(process):
        if process.poll() is None:
            # Unit-fixture safety cleanup for the direct child only; this child
            # is not a Delegate worker, provider, daemon, or native task scope.
            process.kill()
        process.wait(timeout=3)

    def test_failed_spawn_saves_empty_streams_before_raising(self):
        missing = self.root / "missing-delegate"
        args = ["status", "--json"]
        with self.assertRaises(FileNotFoundError) as raised:
            private_cli.run_json(
                missing, self.environment, args, timeout=3, receipt=self.receipt,
                capture_root=self.capture_root,
            )
        self.assertEqual(raised.exception.filename, str(missing))
        self.assert_error_receipt(args, raised.exception, b"", b"", None)

    def test_normal_json_and_exit_policy_keep_existing_receipts(self):
        document = {"message": "café", "count": 3, "complete": True}
        stdout = (json.dumps(document, ensure_ascii=False) + "\n").encode("utf-8")
        stderr = "diagnostic: café\n".encode("utf-8")
        for code, expected in ((0, (0,)), (2, (0, 2)), (7, (0,))):
            with self.subTest(exit_code=code):
                args = self.child_args(stdout, stderr, code)
                if code in expected:
                    result = private_cli.run_json(
                        sys.executable, self.environment, args, expected=expected,
                        timeout=3, receipt=self.receipt, capture_root=self.capture_root,
                    )
                    self.assertEqual(result, document)
                else:
                    with self.assertRaisesRegex(RuntimeError, "exit=7"):
                        private_cli.run_json(
                            sys.executable, self.environment, args, expected=expected,
                            timeout=3, receipt=self.receipt, capture_root=self.capture_root,
                        )
                expected_receipt = {
                    "arguments": args, "exit_code": code,
                    "stdout_bytes": len(stdout),
                    "stdout_sha256": hashlib.sha256(stdout).hexdigest(),
                    "stderr_bytes": len(stderr),
                    "stderr_sha256": hashlib.sha256(stderr).hexdigest(),
                }
                if code != 0:
                    expected_receipt.update({
                        "failure_class": "nonzero_exit",
                        "stdout_tail": private_cli.output_tail(stdout),
                        "stderr_tail": private_cli.output_tail(stderr),
                    })
                self.assertEqual(self.read_receipt(), expected_receipt)

    def test_post_start_error_reaps_direct_control_child_and_retains_capture(self):
        class FailingPollProcess:
            args = ["private-control"]

            def __init__(self):
                self.poll_calls = 0
                self.killed = False
                self.returncode = None

            def poll(self):
                self.poll_calls += 1
                if self.poll_calls == 1:
                    raise OSError("wait failed")
                return self.returncode

            def kill(self):
                self.killed = True
                self.returncode = -9

        process = FailingPollProcess()
        with patch.object(private_cli.subprocess, "Popen", return_value=process):
            with self.assertRaisesRegex(OSError, "wait failed"):
                private_cli.run_captured_command(
                    ["private-control"], self.environment, timeout=3,
                    capture_root=self.capture_root,
                )
        self.assertTrue(process.killed)
        self.assertIsNotNone(process.returncode)
        self.assertEqual(len(tuple(self.capture_root.directory.iterdir())), 2)

    def test_writer_close_error_after_spawn_reaps_direct_control_child(self):
        class CloseFailingWriter:
            def __enter__(self):
                return self

            def __exit__(self, _exc_type, _exc_value, _traceback):
                raise OSError("capture writer close failed")

        class ReapableProcess:
            args = ["private-control"]

            def __init__(self):
                self.killed = False
                self.returncode = None

            def poll(self):
                return self.returncode

            def kill(self):
                self.killed = True
                self.returncode = -9

        process = ReapableProcess()
        with patch.object(
                private_cli, "_open_capture_writer",
                side_effect=[CloseFailingWriter(), CloseFailingWriter()],
        ), patch.object(private_cli.subprocess, "Popen", return_value=process):
            with self.assertRaisesRegex(OSError, "capture writer close failed"):
                private_cli.run_captured_command(
                    ["private-control"], self.environment, timeout=3,
                    capture_root=self.capture_root,
                )
        self.assertTrue(process.killed)
        self.assertEqual(process.returncode, -9)

    @staticmethod
    def open_fd_count():
        for path in (Path("/dev/fd"), Path("/proc/self/fd")):
            if path.is_dir():
                return len(tuple(path.iterdir()))
        raise unittest.SkipTest("the platform does not expose an fd directory")

    def test_capture_writer_constructor_failure_does_not_leak_descriptor(self):
        before = self.open_fd_count()
        with patch.object(io, "open", side_effect=OSError("capture constructor failed")):
            with self.assertRaisesRegex(OSError, "capture constructor failed"):
                private_cli._open_capture_writer(self.capture_root, "constructor-failure.stdout")
        self.assertEqual(self.open_fd_count(), before)

    def test_timeout_primary_survives_capture_read_failure_with_receipt_cause(self):
        class TimedOutProcess:
            def __init__(self, args):
                self.args = args
                self.killed = False
                self.returncode = None

            def poll(self):
                return self.returncode

            def kill(self):
                self.killed = True
                self.returncode = -9

        args = ["status", "--json"]
        process = TimedOutProcess([sys.executable, *args])
        with patch.object(private_cli.subprocess, "Popen", return_value=process), \
                patch.object(
                    private_cli, "_read_capture",
                    side_effect=OSError("capture read failed"),
                ):
            with self.assertRaises(subprocess.TimeoutExpired) as raised:
                private_cli.run_json(
                    sys.executable, self.environment, args, timeout=0,
                    receipt=self.receipt, capture_root=self.capture_root,
                )

        error = raised.exception
        self.assertTrue(process.killed)
        self.assertEqual(process.returncode, -9)
        self.assertIsInstance(error.__cause__, OSError)
        self.assertEqual(str(error.__cause__), "capture read failed")
        evidence = self.read_receipt()
        self.assertEqual(evidence["invocation_error"], str(error))
        self.assertEqual(evidence["stdout_bytes"], 0)
        self.assertEqual(evidence["stderr_bytes"], 0)
        capture_failure = evidence["capture_failure"]
        self.assertEqual(capture_failure["cause"], {
            "type": "OSError", "message": "capture read failed",
        })
        self.assertEqual(
            set(capture_failure["retained_paths"]),
            {str(path) for path in self.capture_root.directory.iterdir()},
        )

    def test_unreaped_cleanup_reports_primary_error_and_retained_capture(self):
        class UnreapedProcess:
            args = ["private-control"]
            returncode = None

            def poll(self):
                return None

            def kill(self):
                pass

        process = UnreapedProcess()
        with patch.object(private_cli.subprocess, "Popen", return_value=process):
            with self.assertRaisesRegex(
                    RuntimeError, "cleanup failed.*retained capture files",
            ) as raised:
                private_cli.run_captured_command(
                    ["private-control"], self.environment, timeout=0,
                    capture_root=self.capture_root,
                )
        self.assertIsInstance(raised.exception.__cause__, subprocess.TimeoutExpired)
        message = str(raised.exception)
        for path in self.capture_root.directory.iterdir():
            self.assertIn(str(path), message)
        self.assertEqual(len(tuple(self.capture_root.directory.iterdir())), 2)

    def test_replaced_capture_root_is_rejected_before_control_spawn(self):
        original = self.capture_root.directory
        moved = self.root / "moved-capture-root"
        original.rename(moved)
        original.mkdir(mode=0o700)
        try:
            with patch.object(private_cli.subprocess, "Popen") as spawn:
                with self.assertRaisesRegex(RuntimeError, "owned directory was replaced"):
                    private_cli.run_captured_command(
                        ["private-control"], self.environment, timeout=3,
                        capture_root=self.capture_root,
                    )
            spawn.assert_not_called()
        finally:
            original.rmdir()
            moved.rename(original)


if __name__ == "__main__":
    unittest.main()
