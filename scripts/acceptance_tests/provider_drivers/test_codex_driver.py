"""Hermetic tests for the Codex acceptance driver oracles.

These tests build no provider and make no network or paid-model call.  Native
live provider behavior is intentionally exercised only by the explicitly invoked gate.
"""

from __future__ import annotations

import json
from pathlib import Path
import shlex
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import scripts.acceptance_codex as gate
import scripts.acceptance_provider_common as provider_common
from scripts.acceptance_supervisor_common import sha
from scripts.delegate_acceptance.provider_drivers.codex import constants as codex_constants
from scripts.delegate_acceptance.provider_drivers.codex import probe as codex_probe
from scripts.delegate_acceptance.provider_drivers.codex import protocol as codex_protocol
from scripts.delegate_acceptance.provider_drivers.codex import runner as codex_runner
from scripts.delegate_acceptance.provider_drivers.codex import runtime as codex_runtime
from scripts.acceptance_provider_common import AcceptanceFailure, wait_runner_done
from scripts.acceptance_supervisor_common import DirectoryOwner


THREAD = "0199a213-81c0-7800-8aa1-bbab2a035a53"
ROOT = "a" * 32
TASK = "b" * 32


def event(value: dict[str, object]) -> bytes:
    return json.dumps(value, ensure_ascii=True, separators=(",", ":")).encode()


def item(item_id: str, item_type: str, **fields: object) -> dict[str, object]:
    return {"id": item_id, "type": item_type, **fields}


def successful_events(nonce_file: Path, inside: Path, sibling: Path, nonce: bytes,
                     final: str = "final answer") -> bytes:
    values = [
        {"type": "thread.started", "thread_id": THREAD},
        {"type": "turn.started"},
        {"type": "item.started", "item": item("reason", "reasoning", text="plan")},
        {"type": "item.completed", "item": item("reason", "reasoning", text="plan")},
        {"type": "item.started", "item": item("old", "agent_message", text="superseded")},
        {"type": "item.completed", "item": item("old", "agent_message", text="superseded")},
        {"type": "item.completed", "item": item(
            "read", "command_execution", command=f"cat -- {nonce_file}",
            aggregated_output=nonce.decode() + "\n", exit_code=0, status="completed")},
        {"type": "item.completed", "item": item(
            "inside", "command_execution", command=f"printf probe > {inside}",
            aggregated_output="permission denied", exit_code=1, status="failed")},
        {"type": "item.completed", "item": item(
            "sibling", "command_execution", command=f"printf probe > {sibling}",
            aggregated_output="", exit_code=None, status="declined")},
        {"type": "item.started", "item": item("final", "agent_message", text=final)},
        {"type": "item.updated", "item": item("final", "agent_message", text=final)},
        {"type": "item.completed", "item": item("final", "agent_message", text=final)},
        {"type": "turn.completed", "status": "completed", "usage": {
            "input_tokens": 10, "cached_input_tokens": 3,
            "cache_write_input_tokens": 1, "output_tokens": 7,
            "reasoning_output_tokens": 2,
        }},
        {"type": "future.nonterminal", "ignored": {"nested": True}},
    ]
    return b"\n".join(event(value) for value in values)


def probe_command(probe: Path) -> str:
    inner = "/bin/sh " + shlex.quote(str(probe))
    return "/bin/zsh -c " + shlex.quote(inner)


def probe_output(nonce: bytes, inside: Path, sibling: Path, workspace_rc: int = 1,
                 sibling_rc: int = 1, workspace_error: str | None = None,
                 sibling_error: str | None = None) -> str:
    if workspace_error is None:
        workspace_error = f"/bin/sh: permission denied: {inside}"
    if sibling_error is None:
        sibling_error = f"/bin/sh: operation not permitted: {sibling}"
    return "\n".join((
        "probe.v1.read_rc=0",
        "probe.v1.nonce=" + nonce.decode(),
        f"probe.v1.workspace_rc={workspace_rc}",
        "probe.v1.workspace_stderr=" + workspace_error,
        f"probe.v1.sibling_rc={sibling_rc}",
        "probe.v1.sibling_stderr=" + sibling_error,
    ))


def probe_events(probe: Path, nonce_file: Path, inside: Path, sibling: Path,
                 nonce: bytes, output: str | None = None) -> bytes:
    output = output or probe_output(nonce, inside, sibling)
    command = probe_command(probe)
    values = [
        {"type": "thread.started", "thread_id": THREAD},
        {"type": "turn.started"},
        {"type": "item.started", "item": item(
            "probe", "command_execution", command=command, aggregated_output="", status="running")},
        {"type": "item.completed", "item": item(
            "probe", "command_execution", command=command, aggregated_output=output,
            exit_code=0, status="completed")},
        {"type": "item.started", "item": item("final", "agent_message", text="probe complete")},
        {"type": "item.completed", "item": item("final", "agent_message", text="probe complete")},
        {"type": "turn.completed", "status": "completed"},
    ]
    return b"\n".join(event(value) for value in values)


def probe_owner(root: Path, nonce: bytes = b"nonce-value-7f3a") -> tuple[object, Path, Path, Path, Path]:
    root = root.resolve()
    state_parent = root / "state-parent"
    state = state_parent / "state"
    workspace = root / "workspace"
    sibling = root / "sibling"
    state.mkdir(parents=True)
    workspace.mkdir()
    sibling.mkdir()
    nonce_file = workspace / "nonce.txt"
    inside = workspace / "workspace-sentinel.txt"
    sibling_file = sibling / "sibling-sentinel.txt"
    nonce_file.write_bytes(nonce + b"\n")
    inside.write_bytes(b"inside\n")
    sibling_file.write_bytes(b"sibling\n")
    probe = state_parent / "probe.sh"
    source = codex_probe.probe_script_bytes(nonce_file, inside, sibling_file)
    probe.write_bytes(source)
    probe.chmod(0o400)
    owner = object.__new__(gate.CodexAcceptance)
    owner.state_parent = state_parent
    owner.state = state
    owner.workspace = workspace
    owner.sibling = sibling
    owner.probe = probe
    owner.probe_size = len(source)
    owner.probe_sha256 = sha(source)
    owner.nonce = nonce
    return owner, probe, nonce_file, inside, sibling_file


def resume_events(nonce: bytes, extra_type: str | None = None,
                  extra_completed: bool = True) -> bytes:
    values: list[dict[str, object]] = [
        {"type": "thread.started", "thread_id": THREAD},
        {"type": "turn.started"},
    ]
    if extra_type is not None:
        fields: dict[str, object] = {"id": "extra", "type": extra_type}
        if extra_type in {"agent_message", "reasoning"}:
            fields["text"] = "thinking"
        elif extra_type == "command_execution":
            fields.update({"command": "/bin/true", "aggregated_output": "",
                           "status": "completed", "exit_code": 0})
        item_event = "item.completed" if extra_completed else "item.started"
        values.append({"type": item_event, "item": fields})
    values.extend([
        {"type": "item.completed", "item": item("answer", "agent_message", text=nonce.decode())},
        {"type": "turn.completed", "status": "completed"},
    ])
    return b"\n".join(event(value) for value in values)


class CodexOracleTests(unittest.TestCase):
    def test_behavioral_success_is_not_published_when_finalization_fails(self):
        for failure in (False, OSError("injected finalization failure")):
            with self.subTest(failure=repr(failure)), \
                    tempfile.TemporaryDirectory(prefix="codex-finalize-unit-") as temporary:
                root = Path(temporary)
                output = root / "evidence"
                output.mkdir()
                owner = object.__new__(gate.CodexAcceptance)
                owner.output = output
                owner.state = root / "state"
                owner.workspace = root / "workspace"
                owner.closed = True
                owner.daemon = None
                owner.dispatch_attempts = set()
                owner.tasks = {"fresh": "a" * 32, "resume": "b" * 32}
                owner.records = {}
                for name in ("fresh", "resume"):
                    directory = root / name
                    directory.mkdir()
                    (directory / "outcome.json").write_text("{}")
                    owner.records[name] = {
                        "reference": {"conversation_id": THREAD},
                        "directory": directory,
                        "outcome": {"evidence_sha256": "c" * 64},
                    }
                owner.setup = lambda: None
                owner.run_fresh = lambda: owner.records["fresh"]
                owner.run_resume = lambda _fresh: owner.records["resume"]
                owner.replay = lambda: None
                owner.shutdown = lambda: None
                owner.safe_failure_shutdown = lambda: True
                calls = []

                def finalize(success):
                    calls.append(success)
                    if success and isinstance(failure, BaseException):
                        raise failure
                    return not success or failure is not False

                owner.finalize = finalize
                argv = ["--tools", str(root), "--pueue", "/pueue", "--pueued", "/pueued",
                        "--output", str(output)]
                with patch.object(gate, "CodexAcceptance", return_value=owner):
                    code = gate.main(argv)

                self.assertEqual(code, 1)
                self.assertEqual(calls, [True, False])
                self.assertFalse((output / "success.json").exists())
                self.assertTrue((output / "failure.json").is_file())
                cleanup = json.loads((output / "failure-cleanup.json").read_text())
                self.assertTrue(cleanup["passed_receipt_absent"])
                self.assertTrue(cleanup["artifact_finalization_completed"])

    def test_event_parser_selects_last_top_level_agent_message(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            nonce_file = root / "nonce.txt"
            inside = root / "inside.txt"
            sibling = root / "sibling.txt"
            nonce = b"nonce-value"
            parsed = codex_protocol.parse_codex_events(successful_events(nonce_file, inside, sibling, nonce))
            self.assertEqual(parsed["thread_id"], THREAD)
            self.assertEqual(parsed["final_message"], b"final answer")
            self.assertEqual(parsed["usage"]["cached_input_tokens"], 3)
            self.assertEqual(len(parsed["commands"]), 3)
            self.assertEqual(parsed["incomplete_commands"], [])

    def test_complete_stream_accepts_nested_multirune_fold_distinct_keys(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            raw = successful_events(root / "n", root / "i", root / "s", b"n")
            raw += b"\n" + event({
                "type": "future.nonterminal",
                "telemetry": {
                    "ß": {"ss": "sharp-s"},
                    "ss": {"ß": "letters"},
                    "ﬀ": {"ff": "ligature"},
                    "ff": {"ﬀ": "letters"},
                },
            })
            parsed = codex_protocol.parse_codex_events(raw)
            self.assertEqual(parsed["thread_id"], THREAD)
            self.assertEqual(parsed["final_message"], b"final answer")

    def test_final_jsonl_line_may_omit_newline(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            raw = successful_events(root / "n", root / "i", root / "s", b"n")
            self.assertEqual(codex_protocol.parse_codex_events(raw)["final_message"], b"final answer")
            self.assertEqual(codex_protocol.parse_codex_events(raw + b"\n")["final_message"], b"final answer")

    def test_unknown_event_is_tolerated_but_terminal_failure_is_not(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            values = [
                {"type": "thread.started", "thread_id": THREAD},
                {"type": "turn.started"},
                {"type": "turn.failed", "error": {"message": "failed"}},
            ]
            raw = b"\n".join(event(value) for value in values)
            with self.assertRaisesRegex(RuntimeError, "successful completed turn"):
                codex_protocol.parse_codex_events(raw)
            malformed = event({"type": "thread.started", "thread_id": THREAD}) + b"\n" + event({
                "type": "turn.started"}) + b"\n" + event({
                "type": "item.completed", "item": item("answer", "agent_message", text="answer")})
            with self.assertRaisesRegex(RuntimeError, "successful completed turn"):
                codex_protocol.parse_codex_events(malformed)
            successful = successful_events(root / "n", root / "i", root / "s", b"n")
            prefix, terminal = successful.rsplit(b"\n", 1)
            unknown_lifecycle = prefix + b"\n" + event({"type": "turn.progress"}) + b"\n" + terminal
            with self.assertRaisesRegex(RuntimeError, "unknown thread/turn lifecycle"):
                codex_protocol.parse_codex_events(unknown_lifecycle)

    def test_pre_turn_completed_error_item_is_a_valid_diagnostic(self):
        values = [
            {"type": "thread.started", "thread_id": THREAD},
            {"type": "item.completed", "item": item(
                "warning", "error", message="configuration warning")},
            {"type": "turn.started"},
            {"type": "item.completed", "item": item(
                "answer", "agent_message", text="recovered")},
            {"type": "turn.completed", "status": "completed"},
        ]
        parsed = codex_protocol.parse_codex_events(b"\n".join(event(value) for value in values))
        self.assertEqual(parsed["final_message"], b"recovered")
        self.assertEqual(parsed["item_states"], [{
            "id": "answer", "type": "agent_message", "started": False,
            "updated": False, "completed": True,
        }])

    def test_agent_message_completion_without_start_is_valid_but_update_requires_start(self):
        values = [
            {"type": "thread.started", "thread_id": THREAD},
            {"type": "turn.started"},
            {"type": "item.completed", "item": item("answer", "agent_message", text="answer")},
            {"type": "turn.completed", "status": "completed"},
        ]
        parsed = codex_protocol.parse_codex_events(b"\n".join(event(value) for value in values))
        self.assertEqual(parsed["final_message"], b"answer")

        values.insert(2, {"type": "item.updated", "item": item("updated", "agent_message", text="answer")})
        with self.assertRaisesRegex(RuntimeError, "item.updated precedes item.started"):
            codex_protocol.parse_codex_events(b"\n".join(event(value) for value in values))

    def test_pre_turn_completed_error_item_requires_a_message(self):
        values = [
            {"type": "thread.started", "thread_id": THREAD},
            {"type": "item.completed", "item": item("warning", "error")},
            {"type": "turn.started"},
            {"type": "item.completed", "item": item(
                "answer", "agent_message", text="answer")},
            {"type": "turn.completed", "status": "completed"},
        ]
        with self.assertRaisesRegex(RuntimeError, "error item message"):
            codex_protocol.parse_codex_events(b"\n".join(event(value) for value in values))

    def test_duplicate_nested_keys_and_blank_final_are_rejected(self):
        duplicate = (b'{"type":"thread.started","thread_id":"' + THREAD.encode() +
                     b'","details":{"x":1,"x":2}}')
        with self.assertRaisesRegex(RuntimeError, "duplicate JSON object key"):
            codex_protocol.parse_codex_events(duplicate)
        with tempfile.TemporaryDirectory():
            values = [
                {"type": "thread.started", "thread_id": THREAD},
                {"type": "turn.started"},
                {"type": "item.completed", "item": item("answer", "agent_message", text=" \n\t")},
                {"type": "turn.completed"},
            ]
            with self.assertRaisesRegex(RuntimeError, "non-whitespace final"):
                codex_protocol.parse_codex_events(b"\n".join(event(value) for value in values))

    def test_resume_oracle_accepts_pure_answer_and_reasoning_only(self):
        nonce = b"resumed-nonce-4f3a"
        parsed = codex_protocol.parse_codex_events(resume_events(nonce))
        result = codex_protocol.validate_tool_free_resume(parsed, nonce)
        self.assertTrue(result["tool_free"])
        self.assertEqual(result["item_types"], ["agent_message"])

        with_reasoning = codex_protocol.parse_codex_events(resume_events(nonce, "reasoning"))
        result = codex_protocol.validate_tool_free_resume(with_reasoning, nonce)
        self.assertEqual(result["item_types"], ["agent_message", "reasoning"])

    def test_resume_oracle_rejects_completed_and_incomplete_tools(self):
        nonce = b"resumed-nonce-4f3a"
        for item_type, completed in (
                ("command_execution", True), ("command_execution", False),
                ("mcp_tool_call", True), ("web_search", False),
                ("collaboration", True), ("file_change", False),
                ("unknown_item_kind", True)):
            with self.subTest(item_type=item_type, completed=completed):
                parsed = codex_protocol.parse_codex_events(resume_events(nonce, item_type, completed))
                with self.assertRaisesRegex(RuntimeError, "disallowed item type"):
                    codex_protocol.validate_tool_free_resume(parsed, nonce)

        values = [json.loads(line) for line in resume_events(nonce).splitlines()]
        values.insert(2, {"type": "mcp_tool_call", "name": "unexpected"})
        parsed = codex_protocol.parse_codex_events(b"\n".join(event(value) for value in values))
        with self.assertRaisesRegex(RuntimeError, "unknown event"):
            codex_protocol.validate_tool_free_resume(parsed, nonce)

    def test_acceptance_receipt_labels_are_neutral_and_turn_count_is_planned(self):
        self.assertEqual(codex_constants.ACCEPTANCE_STATUS, "acceptance-passed")
        self.assertEqual(codex_constants.PRELAUNCH_STATUS, "planned")
        self.assertEqual(codex_constants.PLANNED_NATIVE_AI_TURNS, 2)
        self.assertNotIn("candidate", codex_constants.ACCEPTANCE_STATUS.lower())

    def test_native_codex_binding_omits_removed_config_restrictions(self):
        for removed in ("--ignore-user-config", "--ignore-rules", "--strict-config"):
            self.assertNotIn(removed, codex_constants.RUNTIME_REQUIRED_FLAGS)
        self.assertEqual(codex_constants.OUTPUT_ARGUMENT_INDEX, 11)
        self.assertEqual(codex_constants.NATIVE_PROFILE_REVISION, "native-permissions-v1")
        self.assertIn("DBUS_SESSION_BUS_ADDRESS", codex_constants.CODEX_ENVIRONMENT_KEYS)
        for key in ("GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "GRADLE_USER_HOME", "NPM_CONFIG_CACHE", "GOPATH"):
            self.assertIn(key, codex_constants.CODEX_ENVIRONMENT_KEYS)

    def test_probe_gate_accepts_one_shell_wrapped_probe(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            nonce = b"7ef1a9c0d4"
            owner, probe, nonce_file, inside, sibling = probe_owner(root, nonce)
            parsed = codex_protocol.parse_codex_events(probe_events(probe, nonce_file, inside, sibling, nonce))
            controls = codex_probe.validate_command_execution_controls(
                parsed, probe, inside, sibling, nonce)
            self.assertEqual(controls["read_rc"], 0)
            self.assertEqual(controls["workspace_rc"], 1)
            self.assertEqual(controls["sibling_rc"], 1)
            self.assertEqual(controls["probe_command_id"], "probe")
            owner.require_probe_unchanged("test")

    def test_probe_markers_reject_malformed_missing_and_duplicate_values(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner, probe, nonce_file, inside, sibling = probe_owner(root)
            valid = probe_output(owner.nonce, inside, sibling)
            lines = valid.splitlines()
            with self.assertRaisesRegex(RuntimeError, "incomplete"):
                codex_probe.parse_probe_markers("\n".join(lines[:-1]), owner.nonce, inside, sibling)
            with self.assertRaisesRegex(RuntimeError, "duplicate"):
                codex_probe.parse_probe_markers(valid + "\nprobe.v1.read_rc=0", owner.nonce, inside, sibling)
            malformed = valid.replace("probe.v1.workspace_rc=1", "probe.v1.workspace_rc=one")
            with self.assertRaisesRegex(RuntimeError, "not numeric"):
                codex_probe.parse_probe_markers(malformed, owner.nonce, inside, sibling)
            unknown = valid + "\nprobe.v1.unknown=value"
            with self.assertRaisesRegex(RuntimeError, "unknown marker"):
                codex_probe.parse_probe_markers(unknown, owner.nonce, inside, sibling)

    def test_probe_gate_rejects_wrong_path_and_nonce_print_spoofs(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner, probe, nonce_file, inside, sibling = probe_owner(root)
            parsed = codex_protocol.parse_codex_events(probe_events(probe, nonce_file, inside, sibling, owner.nonce))
            parsed["commands"][0]["command"] = probe_command(root / "other-probe.sh")
            with self.assertRaisesRegex(RuntimeError, "exact immutable"):
                codex_probe.validate_command_execution_controls(parsed, probe, inside, sibling, owner.nonce)

            spoofed = codex_protocol.parse_codex_events(probe_events(probe, nonce_file, inside, sibling, owner.nonce))
            inner = "/bin/sh " + shlex.quote(str(probe)) + "; printf probe.v1.nonce=%s " + owner.nonce.decode()
            spoofed["commands"][0]["command"] = "/bin/zsh -c " + shlex.quote(inner)
            with self.assertRaisesRegex(RuntimeError, "exact immutable"):
                codex_probe.validate_command_execution_controls(spoofed, probe, inside, sibling, owner.nonce)

            quoted_operator = codex_protocol.parse_codex_events(
                probe_events(probe, nonce_file, inside, sibling, owner.nonce))
            quoted_inner = "/bin/sh " + shlex.quote(str(probe)) + " '>'"
            quoted_operator["commands"][0]["command"] = "/bin/zsh -c " + shlex.quote(quoted_inner)
            with self.assertRaisesRegex(RuntimeError, "exact immutable"):
                codex_probe.validate_command_execution_controls(
                    quoted_operator, probe, inside, sibling, owner.nonce)

            command_substitution = codex_protocol.parse_codex_events(
                probe_events(probe, nonce_file, inside, sibling, owner.nonce))
            substitution_inner = "/bin/sh " + shlex.quote(str(probe)) + " $(printf spoof)"
            command_substitution["commands"][0]["command"] = "/bin/zsh -c " + shlex.quote(substitution_inner)
            with self.assertRaisesRegex(RuntimeError, "exact immutable"):
                codex_probe.validate_command_execution_controls(
                    command_substitution, probe, inside, sibling, owner.nonce)

            prose_only = {
                **codex_protocol.parse_codex_events(probe_events(probe, nonce_file, inside, sibling, owner.nonce)),
                "commands": [],
            }
            with self.assertRaisesRegex(RuntimeError, "exactly one"):
                codex_probe.validate_command_execution_controls(prose_only, probe, inside, sibling, owner.nonce)

    def test_probe_gate_rejects_successful_write_and_nonindependent_markers(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner, probe, nonce_file, inside, sibling = probe_owner(root)
            successful_write = codex_protocol.parse_codex_events(probe_events(
                probe, nonce_file, inside, sibling, owner.nonce,
                probe_output(owner.nonce, inside, sibling, workspace_rc=0)))
            with self.assertRaisesRegex(RuntimeError, "workspace write"):
                codex_probe.validate_command_execution_controls(
                    successful_write, probe, inside, sibling, owner.nonce)

            wrong_target = probe_output(owner.nonce, inside, sibling,
                                        sibling_error=f"permission denied: {inside}")
            wrong_target_events = codex_protocol.parse_codex_events(
                probe_events(probe, nonce_file, inside, sibling, owner.nonce, wrong_target))
            with self.assertRaisesRegex(RuntimeError, "wrong absolute path"):
                codex_probe.validate_command_execution_controls(
                    wrong_target_events, probe, inside, sibling, owner.nonce)

            missing_sibling = probe_output(owner.nonce, inside, sibling).replace(
                f"probe.v1.sibling_rc=1\nprobe.v1.sibling_stderr=/bin/sh: operation not permitted: {sibling}", "")
            missing_events = codex_protocol.parse_codex_events(
                probe_events(probe, nonce_file, inside, sibling, owner.nonce, missing_sibling))
            with self.assertRaisesRegex(RuntimeError, "incomplete"):
                codex_probe.validate_command_execution_controls(
                    missing_events, probe, inside, sibling, owner.nonce)

            source = codex_probe.probe_script_bytes(nonce_file, inside, sibling)
            self.assertNotIn(owner.nonce, source)
            self.assertIn(b"workspace_error=$( ( printf", source)
            self.assertIn(b"sibling_error=$( ( printf", source)
            self.assertNotIn(b"&&", source)

    def test_uncompleted_command_items_are_not_control_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner, probe, nonce_file, inside, sibling = probe_owner(root)
            raw = probe_events(probe, nonce_file, inside, sibling, owner.nonce)
            values = [json.loads(line) for line in raw.splitlines()]
            values.insert(4, {"type": "item.started", "item": item(
                "planned-read", "command_execution", command=probe_command(probe),
                aggregated_output=probe_output(owner.nonce, inside, sibling),
                exit_code=0, status="completed")})
            parsed = codex_protocol.parse_codex_events(b"\n".join(event(value) for value in values))
            self.assertNotIn("planned-read", {command.get("id") for command in parsed["commands"]})
            self.assertEqual(parsed["incomplete_commands"], ["planned-read"])
            with self.assertRaisesRegex(RuntimeError, "incomplete command_execution"):
                codex_probe.validate_command_execution_controls(
                    parsed, probe, inside, sibling, owner.nonce)

    def test_read_wrapper_regression_accepts_only_exact_cat(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            nonce_file = root / "nonce with space.txt"
            wrapped = "/bin/zsh -c " + shlex.quote("cat -- " + shlex.quote(str(nonce_file)))
            self.assertTrue(codex_probe.command_reads_path(wrapped, nonce_file))
            spoof = "/bin/zsh -c " + shlex.quote(
                "cat " + shlex.quote(str(nonce_file)) + " >/dev/null; printf nonce")
            self.assertFalse(codex_probe.command_reads_path(spoof, nonce_file))
            self.assertFalse(codex_probe.command_reads_path(
                "/bin/zsh -c " + shlex.quote("cat " + str(nonce_file) + " && echo nonce"), nonce_file))

    def test_probe_wrapper_accepts_bash_login_shell_with_whitespace_path(self):
        with tempfile.TemporaryDirectory() as temporary:
            probe = Path(temporary) / "probe with space.sh"
            inner = "/bin/sh " + shlex.quote(str(probe))
            wrapped = f'/bin/bash -lc "{inner}"'
            self.assertTrue(codex_probe.exact_probe_command(wrapped, probe))
            for flag in ("-x", "--login", "-ilc"):
                rejected = "/bin/bash " + flag + " " + shlex.quote(inner)
                self.assertFalse(codex_probe.exact_probe_command(rejected, probe))

    def test_probe_file_requires_0400_exact_bytes_regular_non_symlink(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner, probe, nonce_file, inside, sibling = probe_owner(root)
            owner.require_probe_unchanged("valid")
            probe.chmod(0o600)
            with self.assertRaisesRegex(RuntimeError, "mode"):
                owner.require_probe_unchanged("mode mutation")
            probe.chmod(0o400)
            probe.chmod(0o600)
            probe.write_bytes(probe.read_bytes() + b"\n")
            probe.chmod(0o400)
            with self.assertRaisesRegex(RuntimeError, "size changed or exceeds"):
                owner.require_probe_unchanged("byte mutation")

            replacement = root / "replacement.sh"
            replacement.write_bytes(codex_probe.probe_script_bytes(nonce_file, inside, sibling))
            replacement.chmod(0o400)
            probe.unlink()
            probe.symlink_to(replacement)
            with self.assertRaisesRegex(RuntimeError, "non-symlink"):
                owner.require_probe_unchanged("symlink mutation")

    def test_transport_does_not_put_prompt_in_argv(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "brief.md"
            path.write_text("quotes ' \" newlines\n$(touch should-not-run)")
            codex_runtime.no_prompt_argv(["delegate", "--brief", str(path)], [path])
            with self.assertRaisesRegex(RuntimeError, "leaked"):
                codex_runtime.no_prompt_argv(["delegate", path.read_text()], [path])
            with self.assertRaisesRegex(RuntimeError, "ephemeral"):
                codex_runtime.no_prompt_argv(["codex", "--ephemeral"], [])

    def test_executable_alias_resolves_to_canonical_regular_file(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            target = root / "codex-real"
            target.write_bytes(b"#!/bin/sh\n")
            target.chmod(0o700)
            alias = root / "codex"
            alias.symlink_to(target)
            selected = codex_runtime.resolve_executable(str(alias), root / "unused", "codex")
            self.assertEqual(selected, target.resolve())
            self.assertFalse(selected.is_symlink())

    def test_runtime_and_temporary_roots_are_rejected(self):
        with self.assertRaisesRegex(RuntimeError, "temporary roots"):
            provider_common.reject_tmp(Path("/tmp/codex-evidence"), "evidence")
        environment = {"HOME": str(Path.home()), "CODEX_HOME": str(Path.home() / ".codex")}
        with self.assertRaisesRegex(RuntimeError, "runtime root"):
            codex_runtime.reject_runtime_roots(Path.home() / ".codex" / "sessions", "state", environment)

    def test_native_cache_roots_are_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for key in ("GRADLE_USER_HOME", "NPM_CONFIG_CACHE"):
                with self.subTest(key=key):
                    cache = root / key.lower()
                    environment = {key: str(cache)}
                    self.assertIn(cache.resolve(), codex_runtime.provider_runtime_roots(environment))
                    self.assertNotIn(cache.resolve(), codex_runtime.provider_runtime_roots({}))
                    with self.assertRaisesRegex(RuntimeError, "runtime root"):
                        codex_runtime.reject_runtime_roots(cache / "state", "state", environment)

    def test_wait_runner_done_requires_one_successful_row(self):
        statuses = [
            {"tasks": {"0": {"label": f"delegate:{ROOT}:{TASK}", "status": "Running"}}},
            {"tasks": {"0": {"label": f"delegate:{ROOT}:{TASK}", "status": {"Done": {"result": "Success"}}}}},
        ]
        calls = []

        def client(name, operation, timeout):
            calls.append((name, operation))
            self.assertGreater(timeout, 0)
            return SimpleNamespace(json=lambda: statuses.pop(0))

        with patch("scripts.acceptance_provider_common.time.sleep") as sleep:
            row = wait_runner_done(client, ROOT, TASK, timeout=150,
                                   sleep_fn=sleep, monotonic_fn=iter([0, 0, 0, 1, 1]).__next__)
        self.assertEqual(row["label"], f"delegate:{ROOT}:{TASK}")
        sleep.assert_called_once_with(0.1)
        self.assertEqual(len(calls), 2)

    def test_wait_runner_done_rejects_failed_or_unrelated_rows(self):
        def failed_client(_name, _operation, timeout):
            self.assertGreater(timeout, 0)
            return SimpleNamespace(json=lambda: {"tasks": {"0": {
                "label": f"delegate:{ROOT}:{TASK}", "status": {"Done": {"result": "Failed"}}}}})

        with self.assertRaisesRegex(AcceptanceFailure, "supervisor result"):
            wait_runner_done(failed_client, ROOT, TASK, timeout=1,
                             monotonic_fn=iter([0, 0, 0]).__next__)

        def unrelated_client(_name, _operation, timeout):
            self.assertGreater(timeout, 0)
            return SimpleNamespace(json=lambda: {"tasks": {"0": {
                "label": "delegate:" + "c" * 32 + ":" + TASK,
                "status": {"Done": {"result": "Success"}}}}})

        with self.assertRaisesRegex(RuntimeError, "no unique"):
            wait_runner_done(unrelated_client, ROOT, TASK, timeout=1,
                             monotonic_fn=iter([0, 0, 0]).__next__)

    def test_failure_shutdown_never_treats_unknown_queue_as_done(self):
        run = object.__new__(gate.CodexAcceptance)
        run.dispatch_attempts = {TASK}
        run.tasks = {"fresh": TASK}
        run.labels = {"fresh": f"delegate:{ROOT}:{TASK}"}
        run.numbers = {"fresh": 0}
        run.runner = Path("/controlled/delegate-run")
        run.state = Path("/controlled/state")
        command = f"{run.runner} --root {run.state} {TASK}"
        done = {"tasks": {"0": {"id": 0, "label": run.labels["fresh"], "group": "default",
                                  "original_command": command,
                                  "command": command, "path": str(run.state),
                                  "status": {"Done": {"result": "Success"}}}}, "groups": {}}
        self.assertTrue(run.failure_queue_finished(done))
        malformed = {"tasks": {"0": {"id": 0, "label": run.labels["fresh"], "group": "default",
                                        "original_command": command,
                                        "command": command, "path": str(run.state),
                                        "status": {"Done": {}}}}, "groups": {}}
        self.assertFalse(run.failure_queue_finished(malformed))
        unknown_result = {"tasks": {"0": {"id": 0, "label": run.labels["fresh"], "group": "default",
                                            "original_command": command,
                                            "command": command, "path": str(run.state),
                                            "status": {"Done": {"result": "Unknown"}}}}, "groups": {}}
        self.assertFalse(run.failure_queue_finished(unknown_result))
        running = {"tasks": {"0": {"id": 0, "label": run.labels["fresh"], "group": "default",
                                     "original_command": command,
                                     "command": command, "path": str(run.state),
                                     "status": "Running"}}, "groups": {}}
        self.assertFalse(run.failure_queue_finished(running))
        missing = {"tasks": {}, "groups": {}}
        self.assertFalse(run.failure_queue_finished(missing))

    def test_diagnostic_failure_does_not_skip_codex_cleanup(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            calls = []
            run = object.__new__(gate.CodexAcceptance)
            run.output = output
            run.state = root / "state"
            run.workspace = root / "workspace"
            run.tasks = {}
            run.dispatch_attempts = set()
            run.daemon = None
            run.setup = lambda: (_ for _ in ()).throw(RuntimeError("primary failure"))
            run.safe_failure_shutdown = lambda: calls.append("shutdown") or True
            run.finalize = lambda success: calls.append(("finalize", success)) or True
            with patch.object(codex_runner, "write_json", side_effect=OSError("diagnostic disk full")):
                with self.assertRaisesRegex(RuntimeError, "primary failure"):
                    run.run()

            self.assertEqual(calls, ["shutdown", ("finalize", False)])

    def test_codex_cleanup_failure_is_fatal_after_primary_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "evidence"
            output.mkdir()
            calls = []
            run = object.__new__(gate.CodexAcceptance)
            run.output = output
            run.state = root / "state"
            run.workspace = root / "workspace"
            run.tasks = {}
            run.dispatch_attempts = set()
            run.daemon = None
            run.setup = lambda: (_ for _ in ()).throw(RuntimeError("primary failure"))
            run.safe_failure_shutdown = lambda: calls.append("shutdown") or True
            run.finalize = lambda success: calls.append(("finalize", success)) or False
            with patch.object(codex_runner, "write_json"):
                with self.assertRaisesRegex(AcceptanceFailure, "cleanup failed"):
                    run.run()

            self.assertEqual(calls, ["shutdown", ("finalize", False)])

    def test_constructor_rolls_back_every_root_created_before_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve(strict=True)
            tools = root / "tools"
            tools.mkdir()
            executables = {}
            for name in ("delegate", "delegate-run", "pueue", "pueued", "codex"):
                executable = tools / name
                executable.write_text("#!/bin/sh\n")
                executable.chmod(0o700)
                executables[name] = executable
            state_parent = root / "states"
            workspace_parent = root / "workspaces"
            pueue_parent = root / "pueue"
            output = root / "evidence"
            args = SimpleNamespace(
                tools=str(tools), delegate=str(executables["delegate"]),
                runner=str(executables["delegate-run"]), pueue=str(executables["pueue"]),
                pueued=str(executables["pueued"]), codex=str(executables["codex"]),
                output=str(output),
            )
            with patch.object(codex_runner, "DEFAULT_STATE_PARENT", state_parent), \
                    patch.object(codex_runner, "DEFAULT_WORKSPACE_PARENT", workspace_parent), \
                    patch.object(codex_runner, "PUEUE_PARENT", pueue_parent), \
                    patch.object(codex_runner, "reject_tmp"), \
                    patch("scripts.delegate_acceptance.provider_drivers.codex.runner.reject_tmp"), \
                    patch.object(codex_runner, "reject_runtime_roots"), \
                    patch.object(codex_runner, "require_discovery"), \
                    patch.object(codex_runner, "write_bytes", side_effect=OSError("injected constructor failure")):
                with self.assertRaisesRegex(OSError, "injected constructor failure"):
                    gate.CodexAcceptance(args)

            self.assertTrue(output.is_dir())
            self.assertFalse(any(state_parent.glob("codex-*")))
            self.assertFalse(any(workspace_parent.glob("codex-*")))

    def test_manifest_and_outcome_helpers_bind_exact_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            payload = b"answer"
            (directory / "result.txt").write_bytes(payload)
            outcome = {"verdict": "committed", "payload": {
                "basename": "result.txt", "length": len(payload), "sha256": sha(payload)}}
            (directory / "outcome.json").write_text(json.dumps(outcome))
            value, data = provider_common.verify_collected_outcome({"outcome": outcome}, directory, "committed")
            self.assertEqual(value, outcome)
            self.assertEqual(data, payload)
            with self.assertRaisesRegex(RuntimeError, "sole outcome"):
                provider_common.verify_collected_outcome({}, directory, "committed")

    def test_finalize_removes_closed_private_roots_and_raw_process_captures(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            state_parent = root / "states"
            workspace_parent = root / "workspaces"
            pueue_parent = root / "pueue"
            output = root / "evidence"
            state = state_parent / "codex-run"
            workspace = workspace_parent / "codex-run"
            sibling = workspace_parent / "codex-run-sibling"
            pueue = pueue_parent / "codex-pueue"
            state_parent.mkdir()
            workspace_parent.mkdir()
            pueue_parent.mkdir()
            output.mkdir()
            directory_owner = DirectoryOwner()
            state_token = directory_owner.create("acceptance state", state, state_parent)
            workspace_token = directory_owner.create("acceptance workspace", workspace, workspace_parent)
            sibling_token = directory_owner.create("acceptance sibling", sibling, workspace_parent)
            raw_token = directory_owner.create("raw process captures", output / "processes", output)
            pueue_token = directory_owner.create("private pueue", pueue, pueue_parent)
            owner = object.__new__(gate.CodexAcceptance)
            owner.output = output
            owner.closed = True
            owner.state_parent = state
            owner.workspace = workspace
            owner.sibling = sibling
            owner.pueue_base = pueue
            owner.owned_directories = (state_token, workspace_token, sibling_token, raw_token)
            owner.pueue_owned_directories = (pueue_token,)
            owner.processes = type("Processes", (), {"capture_root": raw_token})()
            with patch.object(codex_runner, "DEFAULT_STATE_PARENT", state_parent), \
                    patch.object(codex_runner, "DEFAULT_WORKSPACE_PARENT", workspace_parent), \
                    patch.object(codex_runner, "PUEUE_PARENT", pueue_parent):
                self.assertTrue(owner.finalize(True))

            self.assertFalse(state.exists())
            self.assertFalse(workspace.exists())
            self.assertFalse(sibling.exists())
            self.assertFalse(pueue.exists())
            self.assertFalse((output / "processes").exists())


if __name__ == "__main__":
    unittest.main()
