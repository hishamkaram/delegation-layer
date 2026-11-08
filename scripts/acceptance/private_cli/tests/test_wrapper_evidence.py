"""Implementation methods for PrivateWrapperEvidenceTests; collected once by the acceptance package discovery."""

import hashlib
import json
import os
from pathlib import Path
import shlex
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


repository_root = str(Path(__file__).resolve().parents[4])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.directories import operations as directories_operations
from scripts.acceptance.private_cli import configuration as private_cli_configuration
from scripts.acceptance.private_cli import root_cleanup as private_cli_root_cleanup
from scripts.acceptance.private_cli import wrapper_policy as private_cli_wrapper_policy
from scripts.acceptance.private_cli import wrappers as private_cli_wrappers

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.private_cli.handoff import workflow as handoff
from scripts.acceptance.private_cli.capture import run_captured_command
from scripts.acceptance.private_cli.handoff.tasks import HandoffTaskCommands


EXTERNAL_OBSERVATION_PHASE = "external-supervisor-observation"
COMPACT_KEYS = {"installation", "phase", "role", "command", "arguments_bytes", "arguments_sha256"}
REFUSED_EXIT = 125
# Lowers only this child's file-size limit, then replaces itself with the wrapper.
FILE_SIZE_TRAMPOLINE = (
    "import os,resource,sys\n"
    "limit=int(sys.argv[1])\n"
    "resource.setrlimit(resource.RLIMIT_FSIZE,(limit,limit))\n"
    "os.execv(sys.argv[2],sys.argv[2:])\n"
)


def flat(argv):
    return b" ".join(os.fsencode(argument) for argument in argv)


def decoded(test, read):
    """Read a log the test wrote as valid; a refusal of a valid log is an assertion failure."""
    try:
        return read()
    except RuntimeError as error:
        test.fail("valid v1 call log was refused: " + str(error))


def call_line(phase, role, argv):
    """One canonical v1 call record: ASCII JSON with keys v, phase, role, argv in order, then a newline."""
    record = {"v": 1, "phase": phase, "role": role, "argv": list(argv)}
    return json.dumps(record, ensure_ascii=True, separators=(",", ":")).encode("ascii") + b"\n"


class PrivateWrapperEvidenceTests(unittest.TestCase):
    def test_wrapper_calls_are_phase_role_bound_and_compact(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "calls"
            start = ["--config", "/owned/pueue.yml", "start", "--group", "default"]
            path.write_bytes(
                call_line("managed-runner", "client", start)
                + call_line("ordinary-dispatch", "client", ["--config", "/owned/pueue.yml", "add", "--print-task-id"])
            )

            records = decoded(self, lambda: private_cli_wrappers.wrapper_call_records(path, "client", "upgraded"))

            self.assertEqual(records[0]["phase"], "managed-runner")
            self.assertEqual(records[0]["role"], "client")
            self.assertEqual(records[0]["command"], "start")
            self.assertNotIn("arguments", records[0])
            self.assertEqual(records[0]["arguments_bytes"],
                             len("--config /owned/pueue.yml start --group default"))
            with self.assertRaisesRegex(RuntimeError, "startup control command"):
                private_cli_wrapper_policy.require_no_startup_control_calls(records)

    def test_private_root_deletion_requires_the_exact_owned_parent_child(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve(strict=True)
            parent = root / "parent"
            owned = parent / (private_cli_configuration.PRIVATE_ACCEPTANCE_PREFIX + "test")
            owner = directories_operations.DirectoryOwner()
            owned.mkdir(parents=True)
            token = owner.adopt_created("private root", owned, parent)
            entries = owner.transfer()
            private_cli_root_cleanup.remove_private_acceptance_root(token, entries)
            self.assertFalse(owned.exists())


class WrapperRunCase(unittest.TestCase):
    """A real rendered wrapper around a finite callee, run through the canonical capture owner."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate wrapper ü ")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        owner = DirectoryOwner()
        self.capture_root = owner.create("wrapper captures", self.root / "captures", self.root)
        self.addCleanup(owner.rollback)
        self.ran = self.root / "callee-ran"
        self.callee = self.root / "real pueue"
        self.callee.write_text(
            "#!/bin/sh\n"
            ": > " + shlex.quote(str(self.ran)) + "\n"
            "printf '<%s>' \"$@\" \"${FIXTURE_VALUE:-}\"\n"
            "exit \"${FIXTURE_EXIT:-0}\"\n"
        )
        self.callee.chmod(0o700)
        (self.root / "libexec").mkdir()
        (self.root / "logs").mkdir()
        self.marker = self.root / "logs" / "client-starts"
        self.arguments = self.root / "logs" / "client-starts-arguments"
        self.calls = self.root / "logs" / "client-starts-calls"
        self.wrapper = self.root / "libexec" / "pueue"

    def install(self, role="client"):
        private_cli_wrappers.install_wrapper(self.wrapper, self.callee, self.marker, "fixture-install", role)

    def run_wrapper(self, argv, phase=None, file_size_limit=None, wrapper=None, **environment):
        env = {"PATH": "/usr/bin:/bin", **environment}
        if phase is not None:
            env["DELEGATE_ACCEPTANCE_PHASE"] = phase
        command = [str(wrapper or self.wrapper), *argv]
        if file_size_limit is not None:
            command = [sys.executable, "-I", "-c", FILE_SIZE_TRAMPOLINE, str(file_size_limit), *command]
        return run_captured_command(command, env, 15, capture_root=self.capture_root)


class RenderedWrapperTests(WrapperRunCase):
    def test_wrapper_logs_flat_then_canonical_record_and_execs_the_callee_unchanged(self):
        self.install()
        argv = ["--config", "/owned dir/pueue.yml", "status", "--json", "ü\tx"]
        first = self.run_wrapper(argv, phase="managed-runner", FIXTURE_VALUE="kept")
        second = self.run_wrapper(["-c", "/owned/pueue.yml", "add"], FIXTURE_EXIT="7")

        self.assertEqual(first.returncode, 0)
        self.assertEqual(first.stdout, "".join("<%s>" % value for value in [*argv, "kept"]).encode())
        self.assertEqual(second.returncode, 7)
        self.assertEqual(self.marker.read_bytes(), b"started\nstarted\n")
        self.assertEqual(self.arguments.read_bytes(), flat(argv) + b"\n-c /owned/pueue.yml add\n")
        self.assertEqual(
            self.calls.read_bytes(),
            call_line("managed-runner", "client", argv)
            + call_line("unattributed", "client", ["-c", "/owned/pueue.yml", "add"]),
        )
        records = private_cli_wrappers.wrapper_call_records(self.calls, "client", "old")
        self.assertEqual([record["command"] for record in records], ["status", "add"])
        self.assertEqual([record["phase"] for record in records], ["managed-runner", "unattributed"])
        self.assertEqual(records[0]["arguments_bytes"], len(flat(argv)))
        self.assertEqual(records[0]["arguments_sha256"], hashlib.sha256(flat(argv)).hexdigest())
        self.assertTrue(all(set(record) == COMPACT_KEYS for record in records))
        self.assertNotIn("/owned dir/pueue.yml", json.dumps(records))

    def test_daemon_calls_project_startup(self):
        self.install("daemon")
        self.assertEqual(self.run_wrapper(["-c", "/owned/pueue.yml"]).returncode, 0)
        self.assertEqual(self.calls.read_bytes(), call_line("unattributed", "daemon", ["-c", "/owned/pueue.yml"]))
        records = private_cli_wrappers.wrapper_call_records(self.calls, "daemon", "old")
        self.assertEqual([(record["role"], record["command"]) for record in records], [("daemon", "startup")])

    def test_independent_installation_with_spaced_interpreter_keeps_original_logs(self):
        alias = self.root / "python with spaces"
        alias.symlink_to(sys.executable)
        relocated = self.root / "relocated pair"
        relocated.mkdir()
        destination = relocated / "pueue"
        sources = []
        original_write = Path.write_text

        def write_source(path, source, *args, **kwargs):
            if path in (self.wrapper, destination):
                sources.append(source)
            return original_write(path, source, *args, **kwargs)

        def refuse_copy(*_args, **_kwargs):
            self.fail("installed fixture executable was copied")

        with patch("shutil.copyfile", side_effect=refuse_copy), \
                patch.object(sys, "executable", str(alias)), \
                patch.object(Path, "write_text", write_source):
            self.install()
            private_cli_wrappers.install_wrapper(destination, self.callee, self.marker,
                                        "fixture-install", "client")

        result = self.run_wrapper(["status"], phase="relocated", wrapper=destination)

        self.assertEqual(result.returncode, 0)
        self.assertEqual(len(sources), 2)
        self.assertTrue(all(shlex.quote(str(alias)) in source for source in sources))
        self.assertEqual(self.calls.read_bytes(), call_line("relocated", "client", ["status"]))

    def test_flat_open_fault_refuses_before_any_record_or_callee(self):
        self.install()
        self.arguments.mkdir()
        result = self.run_wrapper(["status"])
        self.assertEqual(result.returncode, REFUSED_EXIT)
        self.assertIn(b"pueue wrapper logger refused", result.stderr)
        self.assertFalse(self.calls.exists())
        self.assertFalse(self.ran.exists())

    def test_record_open_fault_refuses_after_the_flat_line_and_before_the_callee(self):
        self.install()
        self.calls.mkdir()
        result = self.run_wrapper(["status"])
        self.assertEqual(result.returncode, REFUSED_EXIT)
        self.assertEqual(self.arguments.read_bytes(), b"status\n")
        self.assertFalse(self.ran.exists())
        with self.assertRaisesRegex(RuntimeError, "not a regular file"):
            private_cli_wrappers.wrapper_call_records(self.calls, "client", "old")

    def test_short_record_append_refuses_without_retry_and_the_torn_log_is_refused(self):
        # Fresh logs only: one oversized record clips at offset 0 on every OS, unlike a prefilled append.
        self.install()
        limit = 4096
        argv = ["status", "--json"]
        phase = "p" * limit
        full = call_line(phase, "client", argv)
        self.assertGreater(len(full), limit)
        result = self.run_wrapper(argv, phase=phase, file_size_limit=limit)
        self.assertEqual(result.returncode, REFUSED_EXIT)
        self.assertEqual(self.arguments.read_bytes(), b"status --json\n")
        self.assertEqual(self.calls.read_bytes(), full[:limit])
        self.assertLess(len(self.calls.read_bytes()), len(full))
        self.assertFalse(self.ran.exists())
        with self.assertRaisesRegex(RuntimeError, "torn final record"):
            private_cli_wrappers.wrapper_call_records(self.calls, "client", "old")

    def test_short_flat_append_refuses_before_the_record_and_callee(self):
        # Fresh logs only: one oversized flat line clips at offset 0 on every OS, unlike a prefilled append.
        self.install()
        limit = 4096
        argv = ["x" * (limit + 1)]
        full = flat(argv) + b"\n"
        self.assertGreater(len(full), limit)
        result = self.run_wrapper(argv, file_size_limit=limit)
        self.assertEqual(result.returncode, REFUSED_EXIT)
        self.assertEqual(self.arguments.read_bytes(), full[:limit])
        self.assertLess(len(self.arguments.read_bytes()), len(full))
        self.assertFalse(self.calls.exists())
        self.assertFalse(self.ran.exists())


class CallLogDecoderTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-wrapper-log-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.calls = self.root / "calls"

    def records(self, data, role="client"):
        self.calls.write_bytes(data)
        return private_cli_wrappers.wrapper_call_records(self.calls, role, "old")

    def valid_records(self, data):
        return decoded(self, lambda: self.records(data))

    def external_commands(self, data, config):
        self.calls.write_bytes(data)
        return HandoffTaskCommands(
            delegate=None, env={}, state=None, workspace=None, waiting_brief=None, receipts=None,
            capture_root=None, client_call_log=self.calls, run_receipt=None, waiter_directory=None,
            lease_nonce=None, external_config=Path(config),
        )

    def external_calls(self, data, config):
        return decoded(self, self.external_commands(data, config).external_client_calls)

    def test_missing_log_has_no_calls_but_other_non_logs_are_faults(self):
        self.assertEqual(private_cli_wrappers.wrapper_call_records(self.calls, "client", "old"), [])
        self.assertEqual(self.valid_records(b""), [])
        target = self.root / "target"
        target.write_bytes(call_line("phase", "client", ["status"]))
        self.calls.unlink()
        self.calls.symlink_to(target)
        with self.assertRaisesRegex(RuntimeError, "not a regular file"):
            private_cli_wrappers.wrapper_call_records(self.calls, "client", "old")

    def test_malformed_records_are_refused(self):
        cases = {
            "torn": call_line("phase", "client", ["status"])[:-1],
            "legacy tab": b"phase\tclient\tstatus\n",
            "non ascii": '{"v":1,"phase":"\u00fc","role":"client","argv":[]}'.encode() + b"\n",
            "duplicate": b'{"v":1,"phase":"p","phase":"p","role":"client","argv":[]}\n',
            "extra key": b'{"v":1,"phase":"p","role":"client","argv":[],"x":1}\n',
            "missing key": b'{"v":1,"phase":"p","role":"client"}\n',
            "key order": b'{"phase":"p","v":1,"role":"client","argv":[]}\n',
            "bool version": b'{"v":true,"phase":"p","role":"client","argv":[]}\n',
            "float version": b'{"v":1.0,"phase":"p","role":"client","argv":[]}\n',
            "other version": b'{"v":2,"phase":"p","role":"client","argv":[]}\n',
            "nan version": b'{"v":NaN,"phase":"p","role":"client","argv":[]}\n',
            "empty phase": b'{"v":1,"phase":"","role":"client","argv":[]}\n',
            "phase type": b'{"v":1,"phase":1,"role":"client","argv":[]}\n',
            "role mismatch": b'{"v":1,"phase":"p","role":"daemon","argv":[]}\n',
            "argv type": b'{"v":1,"phase":"p","role":"client","argv":"status"}\n',
            "argv item": b'{"v":1,"phase":"p","role":"client","argv":[1]}\n',
            "whitespace": b'{"v": 1,"phase":"p","role":"client","argv":[]}\n',
            "lone surrogate": b'{"v":1,"phase":"p","role":"client","argv":["\\ud800"]}\n',
            "bare config": call_line("p", "client", ["-c"]),
            "empty config": call_line("p", "client", ["--config=", "status"]),
        }
        for name, data in cases.items():
            with self.subTest(case=name), self.assertRaises(RuntimeError):
                self.records(call_line("p", "client", ["status"]) + data)

    def test_undecodable_argument_bytes_round_trip_exactly(self):
        argument = os.fsdecode(b"\xff-config")
        records = self.valid_records(call_line("p", "client", ["status", argument]))
        self.assertEqual(records[0]["arguments_sha256"], hashlib.sha256(b"status \xff-config").hexdigest())

    def test_config_selector_matches_each_leading_form_exactly_before_projection(self):
        config = "/state root/.supervisor/pueue.yml"
        selected = [
            ["-c", config, "status", "--json"],
            ["-c" + config, "status"],
            ["--config", config, "status"],
            ["--config=" + config, "status"],
        ]
        excluded = [
            ["status", "--config", config],
            ["-c", config + ".bak", "status"],
            ["-c", "/state root/.supervisor/../.supervisor/pueue.yml", "status"],
            ["-c", "/other.yml", "status", config],
            ["status"],
        ]
        lines = b"".join(call_line(config, "client", argv) for argv in excluded)
        lines += b"".join(call_line("p", "client", argv) for argv in selected)
        records = self.external_calls(lines, config)
        self.assertEqual(len(records), len(selected))
        self.assertEqual({record["command"] for record in records}, {"status"})
        self.assertEqual([record["arguments_sha256"] for record in records],
                         [hashlib.sha256(flat(argv)).hexdigest() for argv in selected])
        self.assertNotIn(config, json.dumps(records))
        self.assertEqual(len(self.valid_records(lines)), len(selected) + len(excluded))

    def test_unsupported_option_first_layouts_are_refused_not_silently_unselected(self):
        config = "/state/.supervisor/pueue.yml"
        layouts = {
            "verbose before config": ["-v", "-c", config, "status"],
            "profile before config": ["-p", "demo", "-c", config, "status"],
            "color before config": ["--color", "never", "-c", config, "status"],
            "unknown option": ["--unknown", "status"],
            "malformed long config": ["--config" + config, "status"],
            "short config with equals": ["-c=" + config, "status"],
            "probe mixed with config": ["--version", "-c", config],
        }
        for name, argv in layouts.items():
            with self.subTest(layout=name):
                with self.assertRaisesRegex(RuntimeError, "unsupported option-first Pueue call layout"):
                    self.records(call_line("p", "client", argv))
                with self.assertRaisesRegex(RuntimeError, "unsupported option-first Pueue call layout"):
                    self.external_commands(call_line("p", "client", argv), config).external_client_calls()

    def test_standalone_probes_and_config_first_version_and_help_stay_ordinary_records(self):
        config = "/state/.supervisor/pueue.yml"
        argvs = [["--version"], ["-V"], ["--help"], ["-h"], ["-c", config, "--version"], ["--config=" + config, "-h"]]
        lines = b"".join(call_line("p", "client", argv) for argv in argvs)
        self.assertEqual([record["command"] for record in self.valid_records(lines)],
                         ["--version", "-V", "--help", "-h", "--version", "-h"])
        self.assertEqual([record["command"] for record in self.external_calls(lines, config)], ["--version", "-h"])

    def test_flat_collision_pair_is_separated_by_the_exact_argv(self):
        spaced = ["-c", "/x y/pueue.yml", "status"]
        split = ["-c", "/x", "y/pueue.yml", "status"]
        self.assertEqual(flat(spaced), flat(split))
        lines = call_line("p", "client", spaced) + call_line("p", "client", split)
        self.assertEqual([record["command"] for record in self.external_calls(lines, "/x y/pueue.yml")], ["status"])
        self.assertEqual([record["command"] for record in self.external_calls(lines, "/x")], ["y/pueue.yml"])


class HandoffExternalCallGuardTests(WrapperRunCase):
    def setUp(self):
        super().setUp()
        self.install()
        self.external = str(self.root / "external supervisor" / "pueue.yml")
        self.private = str(self.root / "state" / ".supervisor" / "pueue.yml")
        self.commands = HandoffTaskCommands(
            delegate=None, env={}, state=None, workspace=None, waiting_brief=None, receipts=None,
            capture_root=self.capture_root, client_call_log=self.calls, run_receipt=None, waiter_directory=None,
            lease_nonce=None, external_config=Path(self.external),
        )

    def transition(self, before):
        case = SimpleNamespace(client=self.wrapper, env={}, capture_root=self.capture_root)
        stop = RuntimeError("guard passed")
        with patch.object(handoff, "pueue_snapshot", side_effect=stop):
            handoff._observe_handoff_transition(
                case=case, evidence={}, external_before_name=None, external_calls_before=before,
                external_pid=None, old_label=None, old_row_before=None, resources=None,
                supervisor_control=SimpleNamespace(external_config=Path(self.external)), task_commands=self.commands,
            )

    def observe_external_task(self, snapshot_logs):
        """Run the handoff's real pre-handoff observation; the fake snapshot optionally logs the real status read."""
        label = "delegate:" + "r" * 32 + ":" + "t" * 32
        receipts = self.root / "receipts"
        receipts.mkdir(exist_ok=True)
        self.commands.receipts = receipts

        def snapshot(client, config, env, phase=None, *, capture_root):
            if snapshot_logs:
                result = self.run_wrapper(["--config", str(config), "status", "--json"], phase=phase)
                self.assertEqual(result.returncode, 0)
            return {"tasks": {"0": {"label": label, "status": {"Running": {}}}}}

        case = SimpleNamespace(client=self.wrapper, env={}, capture_root=self.capture_root)
        with patch.object(self.commands, "task_meta", return_value=(None, b"{}", "m")), \
                patch.object(handoff, "require_handoff_task_binding"), \
                patch.object(handoff, "pueue_snapshot", side_effect=snapshot):
            observed = handoff._observe_external_task(
                case, {"supervisor": {}}, {"root_id": "r" * 32}, "t" * 32,
                SimpleNamespace(external_config=Path(self.external)), self.commands,
            )
        return observed[-1]

    def test_baseline_selects_the_real_external_status_observation(self):
        self.run_wrapper(["-c", self.external, "add", "--", "work"])
        self.run_wrapper(["-c", self.private, "status", "--json"], phase=EXTERNAL_OBSERVATION_PHASE)
        before = self.observe_external_task(snapshot_logs=True)
        self.assertEqual([(record["phase"], record["command"]) for record in before],
                         [("unattributed", "add"), (EXTERNAL_OBSERVATION_PHASE, "status")])

    def test_baseline_without_a_selected_external_observation_is_refused(self):
        self.run_wrapper(["-c", self.private, "status", "--json"], phase=EXTERNAL_OBSERVATION_PHASE)
        self.run_wrapper(["status", "--config", self.external], phase=EXTERNAL_OBSERVATION_PHASE)
        self.run_wrapper(["-c", self.private, "status"], phase="external " + self.external)
        with self.assertRaisesRegex(RuntimeError, "did not select"):
            self.observe_external_task(snapshot_logs=False)

    def test_private_calls_pass_and_an_injected_external_call_fails_the_handoff_guard(self):
        before = self.observe_external_task(snapshot_logs=True)
        self.run_wrapper(["-c", self.private, "status", "--json"])
        with self.assertRaisesRegex(RuntimeError, "guard passed"):
            self.transition(before)
        self.run_wrapper(["--config=" + self.external, "status", "--json"])
        with self.assertRaisesRegex(RuntimeError, "private handoff issued a control call to the external supervisor"):
            self.transition(before)
