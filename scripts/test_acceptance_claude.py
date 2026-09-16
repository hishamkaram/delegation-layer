"""Hermetic Claude acceptance-parser and profile-driver regressions.

The tests exercise only bounded in-memory streams and temporary files.  They
never invoke Claude, the shipped delegate, pueue, or a network service.
"""

from __future__ import annotations

import json
from pathlib import Path
import shlex
import tempfile
import unittest
from unittest.mock import patch

import acceptance_claude as gate
from acceptance_provider_common import AcceptanceFailure, unique_object
from acceptance_supervisor_common import sha


SESSION = "0199a213-81c0-7800-8aa1-bbab2a035a53"
NONCE = b"0123456789abcdef0123456789abcdef0123456789abcdef"


def event(value: dict[str, object]) -> bytes:
    return json.dumps(value, ensure_ascii=True, separators=(",", ":")).encode()


def init_event(session: str = SESSION, tools: list[str] | None = None,
               permission: str = "dontAsk") -> dict[str, object]:
    return {
        "type": "system", "subtype": "init", "session_id": session,
        "claude_code_version": "2.1.270", "cwd": "/workspace",
        "tools": tools or ["Read", "Glob", "Grep"],
        "permissionMode": permission, "apiKeySource": "none", "mcp_servers": [],
    }


def tool_use(identifier: str, name: str, **inputs: object) -> dict[str, object]:
    return {"type": "assistant", "session_id": SESSION,
            "message": {"role": "assistant", "content": [{
                "type": "tool_use", "id": identifier, "name": name, "input": inputs}]}}


def tool_result(identifier: str, content: object, is_error: bool = False) -> dict[str, object]:
    return {"type": "user", "session_id": SESSION,
            "message": {"role": "user", "content": [{
                "type": "tool_result", "tool_use_id": identifier,
                "content": content, "is_error": is_error}]}}


def answer_event(answer: bytes = NONCE, session: str = SESSION) -> dict[str, object]:
    return {"type": "result", "subtype": "success", "is_error": False,
            "session_id": session, "result": answer.decode(), "stop_reason": "end_turn",
            "usage": {"input_tokens": 11, "output_tokens": 3},
            "modelUsage": {"claude-sonnet": {"inputTokens": 11, "outputTokens": 3}}}


def permission_denial(tool_name: str) -> dict[str, object]:
    return {"tool_name": tool_name, "tool_use_id": tool_name.lower() + "-denied",
            "tool_input": {}}


def stream(values: list[dict[str, object]]) -> bytes:
    return b"\n".join(event(value) for value in values)


def fresh_stream(nonce_file: Path, workspace: Path, nonce: bytes = NONCE,
                 denied_tools: tuple[str, ...] = ()) -> bytes:
    values: list[dict[str, object]] = [init_event()]
    values[0]["cwd"] = str(workspace)
    values.extend([
        tool_use("read", "Read", file_path=str(nonce_file)),
        tool_result("read", nonce.decode() + "\n"),
        tool_use("glob", "Glob", pattern=str(workspace / "*"), path=str(workspace)),
        tool_result("glob", [str(workspace / "nonce.txt"), str(workspace / "workspace-sentinel.txt")]),
        tool_use("grep", "Grep", pattern="SENTINEL", path=str(workspace)),
        tool_result("grep", "workspace-sentinel.txt:1:SENTINEL"),
        {"type": "assistant", "session_id": SESSION,
         "message": {"role": "assistant", "content": [{"type": "text", "text": nonce.decode()}]}},
    ])
    result = answer_event(nonce)
    result["permission_denials"] = [permission_denial(name) for name in sorted(denied_tools)]
    values.append(result)
    return stream(values)


class ClaudeOracleTests(unittest.TestCase):
    def test_failure_diagnostic_write_cannot_skip_retained_cleanup(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owner = object.__new__(gate.ClaudeAcceptance)
            owner.output = root / "evidence"
            owner.output.mkdir()
            owner.state = root / "state"
            owner.workspace = root / "workspace"
            owner.tasks = {}
            owner.dispatch_attempts = set()
            owner.daemon = None
            owner.closed = False

            def fail_setup():
                raise RuntimeError("primary acceptance failure")

            retained = []
            owner.setup = fail_setup
            owner.retain_failure_ownership = lambda: retained.append(True) or True
            original_write_json = gate.write_json

            def fail_diagnostics(path, *args, **kwargs):
                if Path(path).name == "failure.json":
                    raise OSError("injected diagnostics write failure")
                return original_write_json(path, *args, **kwargs)

            with patch.object(gate, "write_json", side_effect=fail_diagnostics):
                with self.assertRaisesRegex(RuntimeError, "primary acceptance failure"):
                    owner.run()

            self.assertEqual(retained, [True])
            cleanup = json.loads((owner.output / "failure-cleanup.json").read_text())
            self.assertTrue(cleanup["natural_daemon_shutdown"])
            self.assertFalse(cleanup["failure_diagnostics_written"])

    def test_duplicate_key_oracle_matches_go_simple_fold(self):
        for raw, expected in (
                ('{"ß":1,"ss":2}', {"ß": 1, "ss": 2}),
                ('{"ﬀ":1,"ff":2}', {"ﬀ": 1, "ff": 2})):
            with self.subTest(raw=raw):
                self.assertEqual(json.loads(raw, object_pairs_hook=unique_object), expected)
        for keys in (("K", "K"), ("session_id", "Session_ID")):
            with self.subTest(keys=keys), self.assertRaises(AcceptanceFailure):
                json.loads(json.dumps({keys[0]: 1, keys[1]: 2}),
                           object_pairs_hook=unique_object)
        # U+ A7CE/U+ A7CF is a cased pair added in Unicode 17.0.0.  Python's
        # Unicode 16.0.0 tables do not know this pair, so a Python casefold()
        # based duplicate check would incorrectly accept it.
        with self.assertRaises(AcceptanceFailure):
            json.loads('{"\\uA7CE":1,"\\uA7CF":2}', object_pairs_hook=unique_object)

    def test_parser_requires_init_and_terminal_result(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            raw = fresh_stream(root / "nonce.txt", root)
            parsed = gate.parse_claude_events(raw)
            self.assertEqual(parsed["session_id"], SESSION)
            self.assertEqual(parsed["result_event"]["subtype"], "success")
            self.assertEqual(gate.validate_claude_result(parsed), NONCE)
            with self.assertRaisesRegex(RuntimeError, "system/init|init event"):
                gate.parse_claude_events(stream([answer_event()]))
            with self.assertRaisesRegex(RuntimeError, "terminal result"):
                gate.parse_claude_events(stream([init_event(), {
                    "type": "assistant", "session_id": SESSION,
                    "message": {"role": "assistant", "content": [
                        {"type": "text", "text": "answer"}]}}]))

    def test_result_predicate_rejects_errors_blank_assistant_only_and_bad_stop(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for mutation, message in (
                    (lambda value: value.update({"subtype": "error_max_turns", "is_error": True}), "successful subtype"),
                    (lambda value: value.update({"is_error": True}), "is_error"),
                    (lambda value: value.update({"result": " \n"}), "blank"),
                    (lambda value: value.update({"stop_reason": "max_tokens"}), "incomplete")):
                values = [init_event(), answer_event()]
                mutation(values[-1])
                if message == "is_error":
                    with self.assertRaisesRegex(RuntimeError, "is_error"):
                        gate.parse_claude_events(stream(values))
                    continue
                parsed = gate.parse_claude_events(stream(values))
                with self.assertRaisesRegex(RuntimeError, message):
                    gate.validate_claude_result(parsed)
            with self.assertRaisesRegex(RuntimeError, "nonempty errors"):
                values = [init_event(), answer_event()]
                values[-1]["errors"] = ["warning"]
                gate.parse_claude_events(stream(values))
            with self.assertRaisesRegex(RuntimeError, "substantive output"):
                gate.parse_claude_events(stream([
                    init_event(), answer_event(),
                    {"type": "assistant", "session_id": SESSION,
                     "message": {"role": "assistant", "content": [
                         {"type": "text", "text": "late"}]}}]))

    def test_conflicting_terminal_evidence_cannot_become_success(self):
        for value in (None, 0, "false"):
            result = answer_event()
            result["is_error"] = value
            with self.subTest(is_error=value), self.assertRaisesRegex(RuntimeError, "is_error"):
                gate.parse_claude_events(stream([init_event(), result]))
        for diagnostic in ("", " "):
            result = answer_event()
            result["errors"] = [diagnostic]
            with self.subTest(diagnostic=diagnostic), self.assertRaisesRegex(RuntimeError, "nonempty errors"):
                gate.parse_claude_events(stream([init_event(), result]))
        for subtype in ("interrupted", "error", "cancel"):
            event = {"type": "system", "subtype": subtype, "session_id": SESSION}
            with self.subTest(subtype=subtype), self.assertRaisesRegex(RuntimeError, "lifecycle"):
                gate.parse_claude_events(stream([init_event(), event, answer_event()]))
        for reason in ("interrupted", "abort", "new_stop_reason", "tool_use"):
            result = answer_event()
            result["stop_reason"] = reason
            parsed = gate.parse_claude_events(stream([init_event(), result]))
            with self.subTest(reason=reason), self.assertRaisesRegex(RuntimeError, "stop reason"):
                gate.validate_claude_result(parsed)
        with self.assertRaisesRegex(RuntimeError, "substantive output"):
            gate.parse_claude_events(stream([init_event(), answer_event(), tool_result("late", "late")]))

    def test_duplicate_keys_unknown_event_and_malformed_content_reject(self):
        duplicate = (b'{"type":"system","subtype":"init","session_id":"' +
                     SESSION.encode() + b'","tools":["Read"],"tools":["Read"]}')
        with self.assertRaisesRegex(RuntimeError, "duplicate JSON object key"):
            gate.parse_claude_events(duplicate)
        folded_top_level = (b'{"type":"system","subtype":"init","session_id":"' +
                            SESSION.encode() + b'","Session_ID":"' + SESSION.encode() + b'"}')
        folded_nested = b'{"type":"message","details":{"result":1,"RESULT":2}}'
        for folded in (folded_top_level, folded_nested):
            with self.assertRaisesRegex(RuntimeError, "duplicate JSON object key"):
                gate.parse_claude_events(folded)
        tolerated = gate.parse_claude_events(stream([init_event(), {"type": "message", "x": 1}, answer_event()]))
        self.assertEqual(tolerated["session_id"], SESSION)
        with self.assertRaisesRegex(RuntimeError, "lifecycle"):
            gate.parse_claude_events(stream([init_event(), {"type": "turn.progress"}, answer_event()]))
        trailing_identity = {"type": "system", "subtype": "status",
                             "session_id": "0199a213-81c0-7800-8aa1-bbab2a035a54"}
        with self.assertRaisesRegex(RuntimeError, "identity|precedes system/init"):
            gate.parse_claude_events(stream([init_event(), answer_event(), trailing_identity]))
        bad = tool_use("x", "Read", file_path="/x")
        bad["message"]["content"][0]["input"] = []
        with self.assertRaisesRegex(RuntimeError, "input is not an object"):
            gate.parse_claude_events(stream([init_event(), bad, answer_event()]))

    def test_init_profile_is_exact_and_reports_api_key_source(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            parsed = gate.parse_claude_events(stream([init_event(), answer_event()]))
            profile = gate.validate_init_profile(parsed, SESSION)
            self.assertEqual(profile["tools"], ["Read", "Glob", "Grep"])
            self.assertEqual(profile["permission_mode"], "dontAsk")
            self.assertEqual(parsed["init"].get("apiKeySource"), "none")
            for tools in (["Read", "Glob", "Grep", "Bash"], ["Read", "Glob"], ["Read", "Read", "Glob", "Grep"]):
                changed = init_event(tools=list(tools))
                changed_stream = gate.parse_claude_events(stream([changed, answer_event()]))
                with self.assertRaisesRegex(RuntimeError, "tool (surface|list)"):
                    gate.validate_init_profile(changed_stream)
            changed = gate.parse_claude_events(stream([init_event(permission="acceptEdits"), answer_event()]))
            with self.assertRaisesRegex(RuntimeError, "permission mode"):
                gate.validate_init_profile(changed)
            alias = init_event()
            alias["permission_mode"] = alias.pop("permissionMode")
            with self.assertRaisesRegex(RuntimeError, "permission mode"):
                gate.validate_init_profile(gate.parse_claude_events(stream([alias, answer_event()])))
            changed = init_event()
            changed["mcp_servers"] = [{"name": "unexpected"}]
            with self.assertRaisesRegex(RuntimeError, "mcp_servers"):
                gate.validate_init_profile(gate.parse_claude_events(stream([changed, answer_event()])))
            for source in ("env", None):
                changed = init_event()
                if source is None:
                    del changed["apiKeySource"]
                else:
                    changed["apiKeySource"] = source
                with self.assertRaisesRegex(RuntimeError, "apiKeySource"):
                    gate.validate_init_profile(
                        gate.parse_claude_events(stream([changed, answer_event()])))

    def test_fresh_controls_require_actual_read_glob_grep_and_blocked_routes(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            workspace = root / "workspace"
            sibling = root / "sibling"
            workspace.mkdir()
            sibling.mkdir()
            nonce_file = workspace / "nonce.txt"
            parsed = gate.parse_claude_events(fresh_stream(nonce_file, workspace))
            controls = gate.validate_fresh_controls(parsed, nonce_file, workspace, sibling, NONCE)
            self.assertTrue(controls["nonce_read"])
            self.assertEqual(set(controls["blocked_tools"]), gate.REQUIRED_BLOCKED_TOOLS)
            self.assertEqual(
                controls["blocked_tool_evidence"],
                {name: "unavailable_init_tools" for name in gate.REQUIRED_BLOCKED_TOOLS},
            )
            for name in ("Read", "Glob", "Grep"):
                mutated = gate.parse_claude_events(fresh_stream(nonce_file, workspace))
                uses = [use for use in mutated["tool_uses"] if use["name"] == name]
                uses[0]["input"] = {"path": str(root / "other")}
                with self.assertRaisesRegex(RuntimeError, "exactly one nonce Read|positive .*workspace"):
                    gate.validate_fresh_controls(mutated, nonce_file, workspace, sibling, NONCE)
            blocked = fresh_stream(nonce_file, workspace).decode().replace(
                '"tools":["Read","Glob","Grep"]', '"tools":["Read","Glob","Grep","Bash"]')
            with self.assertRaisesRegex(RuntimeError, "tool surface"):
                gate.validate_fresh_controls(gate.parse_claude_events(blocked.encode()), nonce_file, workspace, sibling, NONCE)
            wrong_session = gate.fresh_session_id(
                "fedcba9876543210fedcba9876543210",
                "0123456789abcdef0123456789abcdef",
            )
            with self.assertRaisesRegex(RuntimeError, "session identity mismatch"):
                gate.validate_fresh_controls(
                    parsed, nonce_file, workspace, sibling, NONCE,
                    expected_session=wrong_session,
                )
            denied = gate.parse_claude_events(
                fresh_stream(nonce_file, workspace, denied_tools=("Bash",)))
            denied_controls = gate.validate_fresh_controls(
                denied, nonce_file, workspace, sibling, NONCE)
            self.assertEqual(denied_controls["blocked_tool_evidence"]["Bash"],
                             "permission_denied")
            self.assertEqual(denied_controls["blocked_tool_evidence"]["Write"],
                             "unavailable_init_tools")
            values = [json.loads(line) for line in fresh_stream(nonce_file, workspace).splitlines()]
            for value in values:
                if value.get("type") == "user" and value["message"]["content"][0].get("tool_use_id") == "glob":
                    value["message"]["content"][0]["content"] = []
            with self.assertRaisesRegex(RuntimeError, "Glob returned an empty result"):
                gate.validate_fresh_controls(gate.parse_claude_events(stream(values)), nonce_file, workspace, sibling, NONCE)

    def test_fresh_controls_reject_tool_failure_and_nonce_prose(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            workspace = root / "workspace"
            sibling = root / "sibling"
            workspace.mkdir()
            sibling.mkdir()
            nonce_file = workspace / "nonce.txt"
            values = [json.loads(line) for line in fresh_stream(nonce_file, workspace).splitlines()]
            for value in values:
                if value.get("type") == "user" and value["message"]["content"][0].get("tool_use_id") == "read":
                    value["message"]["content"][0]["is_error"] = True
            with self.assertRaisesRegex(RuntimeError, "tool failed"):
                gate.validate_fresh_controls(gate.parse_claude_events(stream(values)), nonce_file, workspace, sibling, NONCE)
            values = [value for value in values if not (
                value.get("type") == "user" and value["message"]["content"][0].get("tool_use_id") == "read")]
            with self.assertRaisesRegex(RuntimeError, "unique result"):
                gate.validate_fresh_controls(gate.parse_claude_events(stream(values)), nonce_file, workspace, sibling, NONCE)

    def test_resume_is_tool_free_and_exact_identity(self):
        values = [init_event(), {"type": "assistant", "session_id": SESSION,
                                 "message": {"role": "assistant", "content": [
                                     {"type": "thinking", "thinking": "recall"}]}}, answer_event()]
        parsed = gate.parse_claude_events(stream(values))
        result = gate.validate_tool_free_resume(parsed, NONCE, SESSION)
        self.assertTrue(result["tool_free"])
        self.assertEqual(result["item_types"], ["thinking"])
        for bad in (tool_use("x", "Read", file_path="/tmp/n"),
                    tool_use("x", "Bash", command="true"),
                    tool_result("x", "denied", is_error=True)):
            values = [init_event(), bad, answer_event()]
            if bad["type"] == "user":
                # A tool result without a corresponding tool use is still a
                # tool-bearing continuation and must fail closed.
                pass
            with self.assertRaisesRegex(RuntimeError, "tool"):
                gate.validate_tool_free_resume(gate.parse_claude_events(stream(values)), NONCE, SESSION)
        with self.assertRaisesRegex(RuntimeError, "unexpected continuation answer"):
            gate.validate_tool_free_resume(gate.parse_claude_events(stream([init_event(), answer_event(b"other")])), NONCE)
        denied = answer_event()
        denied["permission_denials"] = [permission_denial("Bash")]
        with self.assertRaisesRegex(RuntimeError, "permission-denial"):
            gate.validate_tool_free_resume(
                gate.parse_claude_events(stream([init_event(), denied])), NONCE, SESSION)

    def test_parser_binds_preinit_identity_and_rejects_out_of_profile_tools(self):
        preinit = {"type": "user", "session_id": "0199a213-81c0-7800-8aa1-bbab2a035a54",
                   "message": {"content": []}}
        with self.assertRaisesRegex(RuntimeError, "identity|precedes system/init"):
            gate.parse_claude_events(stream([preinit, init_event(), answer_event()]))
        blocked = tool_use("blocked", "Bash", command="true")
        with self.assertRaisesRegex(RuntimeError, "outside the restricted tool profile"):
            gate.parse_claude_events(stream([init_event(), blocked, answer_event()]))

    def test_nullable_usage_envelopes_are_absent_and_fresh_session_is_uuidv5(self):
        result = answer_event()
        result["usage"] = None
        result["modelUsage"] = None
        parsed = gate.parse_claude_events(stream([init_event(), result]))
        self.assertIsNone(parsed["usage"])
        self.assertIsNone(parsed["model_usage"])
        self.assertEqual(gate.fresh_session_id(
            "fedcba9876543210fedcba9876543210", "0123456789abcdef0123456789abcdef"),
            "99b93a5f-a02f-5975-8eea-ea1dd72facb2")

    def test_empty_model_usage_does_not_invent_records(self):
        result = answer_event()
        result.pop("usage", None)
        result["modelUsage"] = {}
        parsed = gate.parse_claude_events(stream([init_event(), result]))
        for outcome in ({}, {"usage": None}, {"usage": []}):
            gate.validate_record_usage(outcome, parsed, "empty-model-usage")
        with self.assertRaisesRegex(AcceptanceFailure, "invented usage"):
            gate.validate_record_usage({"usage": [{}]}, parsed, "empty-model-usage")

    def test_record_usage_preserves_native_scopes_and_nullable_fields(self):
        init = init_event()
        init["model"] = "claude-test"
        result = answer_event()
        result.update({"duration_ms": 1500, "num_turns": 1, "total_cost_usd": 0.0012,
                       "usage": {"input_tokens": 11, "output_tokens": 3,
                                  "thinking_tokens": None, "total_tokens": None},
                       "modelUsage": {"claude-sonnet": {"inputTokens": 11,
                                                           "outputTokens": 3,
                                                           "costUSD": None}}})
        parsed = gate.parse_claude_events(stream([init, result]))
        outcome = {"usage": [
            {"scope": "main-agent", "source": "provider-envelope", "reliability": "reported",
             "input_tokens": 11, "output_tokens": 3, "num_turns": 1,
             "duration_seconds": "1.5", "model": "claude-test"},
            {"scope": "whole-tree", "source": "provider-envelope", "reliability": "reported",
             "estimated_cost_usd": "0.0012"},
            {"scope": "whole-tree", "source": "provider-envelope", "reliability": "reported",
             "model": "claude-sonnet", "input_tokens": 11, "output_tokens": 3},
        ]}
        gate.validate_record_usage(outcome, parsed, "usage")
        outcome["usage"][0]["input_tokens"] = False
        with self.assertRaisesRegex(RuntimeError, "input_tokens"):
            gate.validate_record_usage(outcome, parsed, "usage")
        outcome["usage"][0]["input_tokens"] = 11
        outcome["usage"].pop()
        with self.assertRaisesRegex(RuntimeError, "omitted or synthesized"):
            gate.validate_record_usage(outcome, parsed, "usage")

    def test_manifest_digest_covers_canonical_descriptors(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            raw = directory / "raw"
            raw.mkdir()
            stdout = raw / "stdout"
            stderr = raw / "stderr"
            stdout.write_bytes(b"stdout")
            stderr.write_bytes(b"")
            manifest = [
                {"path": "raw/stderr", "size": 0, "sha256": sha(b"")},
                {"path": "raw/stdout", "size": 6, "sha256": sha(b"stdout")},
            ]
            canonical = json.dumps(manifest, separators=(",", ":")) + "\n"
            seal = {"raw_manifest": manifest, "manifest_sha256": sha(canonical.encode())}
            gate.validate_manifest(directory, seal)
            seal["manifest_sha256"] = sha(b"arbitrary")
            with self.assertRaisesRegex(RuntimeError, "manifest_sha256"):
                gate.validate_manifest(directory, seal)

    def test_provider_argv_fresh_resume_parity_and_prompt_transport(self):
        owner = object.__new__(gate.ClaudeAcceptance)
        owner.claude = Path("/opt/homebrew/Caskroom/claude-code@latest/2.1.270/claude")
        owner.profile = Path("/state/claude-profile.json")
        owner.empty_mcp = Path("/state/empty-mcp.json")
        fresh = owner.provider_argv(SESSION)
        resume = owner.provider_argv(SESSION, resume=True)
        self.assertIn("--session-id", fresh)
        self.assertNotIn("--resume", fresh)
        self.assertIn("--resume", resume)
        self.assertNotIn("--session-id", resume)
        self.assertEqual(fresh[:-2], resume[:-2])
        self.assertIn("mcp__*", fresh)
        self.assertIn("--safe-mode", fresh)
        self.assertIn("--restricted", fresh)
        with tempfile.TemporaryDirectory() as temporary:
            brief = Path(temporary) / "brief.md"
            brief.write_text("sensitive prompt $(touch nope)")
            no_prompt_argv = gate.no_prompt_argv
            no_prompt_argv(["delegate", "--brief", str(brief)], [brief])
            with self.assertRaisesRegex(RuntimeError, "leaked"):
                no_prompt_argv(["delegate", brief.read_text()], [brief])

    def test_expected_inspection_binding_matches_native_sources_and_go_hashes(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            base = root / "pueue"
            (base / "state").mkdir(parents=True)
            (base / "run").mkdir()
            config = root / "pueue.yml"
            gate.write_json(config, gate.config_for(base))
            pueue = root / "pueue-bin"
            runner = root / "delegate-run"
            helper = root / "security"
            claude = root / "claude"
            for path, content in ((pueue, b"pueue"), (runner, b"runner"),
                                  (helper, b"helper"), (claude, b"claude")):
                path.write_bytes(content)
                path.chmod(0o700)
            home = root / "home"
            home.mkdir()
            workspace = root / "workspace"
            workspace.mkdir()
            environment = {"HOME": str(home), "PATH": "/usr/bin", "USER": "fixture-user", "LANG": "C"}

            binding = gate.expected_claude_inspection_binding(
                pueue, config, base, gate.digest(config), workspace, claude,
                runner, environment, helper)
            definition = {
                "revision": gate.NATIVE_INSPECTION_REVISION,
                "executable": str(helper.resolve()),
                "executable_sha256": gate.digest(helper),
                "arguments": ["find-generic-password", "-s", "Claude Code-credentials",
                              "-a", "fixture-user", "-w"],
                "directory": str(home.resolve()),
                "environment": ["CLAUDE_CODE_HOVER_REST=0", "HOME=" + str(home),
                                "LANG=C", "PATH=/usr/bin", "USER=fixture-user"],
                "output_limit": 1 << 20,
                "runtime": {
                    "executable": str(claude.resolve()),
                    "executable_sha256": gate.digest(claude),
                    "directory": str(workspace.resolve()),
                    "environment": ["CLAUDE_CODE_HOVER_REST=0", "HOME=" + str(home),
                                    "LANG=C", "PATH=/usr/bin", "USER=fixture-user"],
                    "help_args": None,
                    "required_flags": list(gate.RUNTIME_REQUIRED_FLAGS),
                },
                "remote": {
                    "url": gate.NATIVE_POLICY_ENDPOINT,
                    "headers": {"Cache-Control": "no-cache", "Pragma": "no-cache",
                                 "User-Agent": "claude-cli (external, cli)",
                                 "anthropic-beta": "oauth-2025-04-20"},
                },
            }
            self.assertEqual(binding["definition_sha256"],
                             sha(gate.canonical_go_json(definition)))
            self.assertNotEqual(binding["definition_sha256"],
                                sha(gate.canonical_go_json(definition, newline=False)))
            self.assertEqual(binding["helper_executable"], str(helper.resolve()))
            self.assertEqual(binding["worker_executable"], str(runner.resolve()))
            self.assertEqual(binding["supervisor"]["config_digest"], gate.digest(config))
            self.assertEqual(binding["supervisor"]["endpoint"],
                             "unix:" + str(base.resolve() / "run" / "p.sock"))

    def test_expected_inspection_binding_mirrors_account_fallback_and_rejects_selectors(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            base = root / "pueue"
            (base / "state").mkdir(parents=True)
            (base / "run").mkdir()
            config = root / "pueue.yml"
            gate.write_json(config, gate.config_for(base))
            paths = []
            for name in ("pueue-bin", "delegate-run", "security"):
                path = root / name
                path.write_bytes(name.encode())
                path.chmod(0o700)
                paths.append(path)
            home = root / "home"
            home.mkdir()
            environment = {"HOME": str(home), "PATH": "/usr/bin", "USER": "bad/user"}
            self.assertEqual(gate._native_account(environment), "claude-code-user")
            with self.assertRaisesRegex(gate.BlockedFailure, "alternate Claude environment"):
                gate.expected_claude_inspection_binding(
                    paths[0], config, base, gate.digest(config), home, paths[2], paths[1],
                    {**environment, "ANTHROPIC_API_KEY": "ambient"}, paths[2])

    def test_dispatch_registers_complete_expected_binding_before_admission(self):
        class FakeOps:
            root_id = None

            def __init__(self):
                self.expected = None

            def expect_inspection(self, task, binding):
                self.expected = task, binding

            def dispatch(self, name, task, argv):
                self.dispatched = name, task, argv
                return {"root_id": "a" * 32}

        owner = object.__new__(gate.ClaudeAcceptance)
        owner.root_id = None
        owner.inspection_binding = {
            "definition_revision": gate.NATIVE_INSPECTION_REVISION,
            "definition_sha256": "a" * 64,
            "helper_executable": "/fixture/security",
            "helper_sha256": "b" * 64,
            "worker_executable": "/fixture/runner",
            "worker_sha256": "c" * 64,
            "supervisor": {"expected": True},
        }
        owner.ops = FakeOps()
        owner.dispatch_arguments = lambda task, brief, predecessor=None: ["delegate", task]
        binding = owner.inspection_binding
        response = owner.dispatch("fresh", "b" * 32, Path("/fixture/brief"))
        self.assertEqual(response["root_id"], "a" * 32)
        self.assertEqual(owner.ops.expected, ("b" * 32, binding))

    def test_executable_alias_and_runtime_roots(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            target = root / "claude-real"
            target.write_bytes(b"#!/bin/sh\n")
            target.chmod(0o700)
            alias = root / "claude"
            alias.symlink_to(target)
            self.assertEqual(gate.resolve_executable(str(alias), root / "unused", "claude"), target.resolve())
        with self.assertRaisesRegex(RuntimeError, "temporary roots"):
            gate.reject_tmp(Path("/tmp/claude-evidence"), "evidence")
        environment = {"HOME": str(Path.home())}
        with self.assertRaisesRegex(RuntimeError, "runtime root"):
            gate.reject_runtime_roots(Path.home() / ".claude" / "sessions", "state", environment)

    def test_live_profile_environment_is_task_owned(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            home = root / "home"
            scratch = root / "tmp"
            inherited = {
                "HOME": "/real/user",
                "PATH": "/usr/bin",
                "USER": "fixture-user",
                "CLAUDE_CODE_COZY_TEAPOT": "relaxed",
                "ANTHROPIC_API_KEY": "ambient-secret",
                "XDG_CONFIG_HOME": "/real/config",
                "TMPDIR": "/real/tmp",
            }
            isolated = gate.isolated_acceptance_environment(inherited, home, scratch)
            self.assertEqual(isolated["HOME"], str(home.resolve()))
            self.assertEqual(isolated["TMPDIR"], str(scratch.resolve()))
            self.assertEqual(isolated["TMP"], str(scratch.resolve()))
            self.assertEqual(isolated["TEMP"], str(scratch.resolve()))
            self.assertNotIn("CLAUDE_CODE_COZY_TEAPOT", isolated)
            self.assertNotIn("ANTHROPIC_API_KEY", isolated)
            self.assertNotIn("XDG_CONFIG_HOME", isolated)
            validation = gate.path_validation_environment(inherited)
            self.assertEqual(validation["HOME"], "/real/user")
            self.assertEqual(validation["TMPDIR"], "/real/tmp")
            self.assertNotIn("ANTHROPIC_API_KEY", validation)
            self.assertNotIn("CLAUDE_CODE_COZY_TEAPOT", validation)
            self.assertNotIn("XDG_CONFIG_HOME", validation)

    def test_native_account_environment_keeps_login_and_relocates_mutable_temp(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            home = root / "home"
            home.mkdir()
            scratch = root / "tmp"
            scratch.mkdir()
            inherited = {
                "HOME": "/real/user",
                "PATH": "/usr/bin",
                "USER": "fixture-user",
                "TMPDIR": "/real/tmp",
                "ANTHROPIC_API_KEY": "ambient-secret",
            }
            environment = gate.native_account_acceptance_environment(inherited, home, scratch)
            self.assertEqual(environment["HOME"], str(home.resolve()))
            self.assertEqual(environment["TMPDIR"], str(scratch.resolve()))
            self.assertEqual(environment["TMP"], str(scratch.resolve()))
            self.assertEqual(environment["TEMP"], str(scratch.resolve()))
            self.assertNotIn("ANTHROPIC_API_KEY", environment)

    def test_plaintext_fallback_observation_uses_metadata_only(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            absent = root / "missing" / ".credentials.json"
            self.assertFalse(gate.observe_plaintext_fallback(absent))
            present = root / "present" / ".credentials.json"
            present.parent.mkdir()
            present.write_bytes(b"synthetic credential bytes")
            self.assertTrue(gate.observe_plaintext_fallback(present))
            target = root / "target"
            target.write_bytes(b"synthetic credential bytes")
            link = root / "link"
            link.symlink_to(target)
            self.assertTrue(gate.observe_plaintext_fallback(link))

    def test_acceptance_receipt_labels_are_neutral(self):
        self.assertEqual(gate.ACCEPTANCE_STATUS, "acceptance-passed")
        self.assertEqual(gate.PRELAUNCH_STATUS, "planned")
        self.assertNotIn("candidate", gate.ACCEPTANCE_STATUS.lower())


if __name__ == "__main__":
    unittest.main()
