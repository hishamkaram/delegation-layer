"""Claude provider stream protocol predicates."""

from __future__ import annotations

from copy import deepcopy
from collections.abc import Mapping
from dataclasses import dataclass, field
from decimal import Decimal, InvalidOperation
import json
from typing import TypedDict
from pathlib import Path
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.evidence.json_decoder import unique_object
from scripts.delegate_acceptance.provider_drivers.claude.constants import (
    APPROVAL,
    ERROR_SUBTYPES,
    KNOWN_CONTENT_TYPES,
    MAX_CONTROL_BYTES,
    MAX_PROVIDER_BYTES,
)

from scripts.delegate_acceptance.provider_drivers.claude.identity import is_session_id


class InitProfile(TypedDict):
    session_id: str
    tools: list[str]
    permission_mode: str
    api_key_source: str
    mcp_server_count: int


class ParsedStream(TypedDict):
    session_id: str
    init: dict[str, object]
    result_event: dict[str, object]
    events: list[dict[str, object]]
    tool_uses: list[dict[str, object]]
    tool_results: list[dict[str, object]]
    permission_denials: list[dict[str, object]]
    usage: object
    model_usage: object


def _content_blocks(event: dict[str, object]) -> list[dict[str, object]]:
    message = event.get("message")
    if message is None:
        return []
    if not (isinstance(message, dict)):
        raise AcceptanceFailure("Claude message event has no message object")
    content = message.get("content")
    if content is None:
        return []
    if not (isinstance(content, list)):
        raise AcceptanceFailure("Claude message content is not an array")
    blocks: list[dict[str, object]] = []
    for block in content:
        if not (isinstance(block, dict)):
            raise AcceptanceFailure("Claude message content block is not an object")
        kind = block.get("type")
        if not (isinstance(kind, str) and kind in KNOWN_CONTENT_TYPES):
            raise AcceptanceFailure("unknown Claude content block type")
        if kind == "text":
            if not (isinstance(block.get("text"), str)):
                raise AcceptanceFailure("Claude text block has no text")
        elif kind == "thinking":
            if not (isinstance(block.get("thinking"), str)):
                raise AcceptanceFailure("Claude thinking block has no thinking text")
        elif kind == "redacted_thinking":
            if not (any(isinstance(block.get(field), str) for field in ("data", "text", "thinking"))):
                raise AcceptanceFailure("Claude redacted thinking block has no data")
        elif kind == "tool_use":
            if not (isinstance(block.get("id"), str) and block["id"]):
                raise AcceptanceFailure("Claude tool_use has no id")
            if not (isinstance(block.get("name"), str) and block["name"]):
                raise AcceptanceFailure("Claude tool_use has no name")
            if not (isinstance(block.get("input"), dict)):
                raise AcceptanceFailure("Claude tool_use input is not an object")
        else:
            if not (isinstance(block.get("tool_use_id"), str) and block["tool_use_id"]):
                raise AcceptanceFailure("Claude tool_result has no tool_use_id")
            if "is_error" in block:
                if not (isinstance(block["is_error"], bool)):
                    raise AcceptanceFailure("Claude tool_result is_error is not boolean")
            content_value = block.get("content", "")
            if not (content_value is None or isinstance(content_value, (str, list, dict))):
                raise AcceptanceFailure("Claude tool_result content has an invalid shape")
        blocks.append(block)
    return blocks
def _content_text(value: object) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "\n".join(_content_text(entry) for entry in value)
    if isinstance(value, dict):
        return "\n".join(_content_text(entry) for entry in value.values())
    return ""
def _counter(value: object, label: str) -> None:
    if value is None:
        return
    if not (isinstance(value, int) and not isinstance(value, bool) and 0 <= value <= (1 << 63) - 1):
        raise AcceptanceFailure(f"Claude {label} is not a bounded nonnegative integer")
def _decimal(value: object, label: str) -> None:
    if value is None:
        return
    if not (isinstance(value, (int, float, Decimal)) and not isinstance(value, bool)):
        raise AcceptanceFailure(f"Claude {label} is not a JSON number")
    try:
        number = Decimal(str(value))
    except (InvalidOperation, TypeError, ValueError) as error:
        raise AcceptanceFailure(f"Claude {label} is not a JSON number") from error
    require(number.is_finite() and number >= 0, f"Claude {label} is not finite and nonnegative")
def _nonce_tool_uses(results: list[dict[str, object]], uses: list[dict[str, object]],
                     nonce_file: Path, nonce: bytes) -> list[dict[str, object]]:
    nonce_text = nonce.decode("ascii")
    matches: list[dict[str, object]] = []
    for use in uses:
        if not _tool_input_mentions_path(use, nonce_file):
            continue
        result = _successful_tool_result(results, use)
        content = _content_text(result.get("content"))
        if nonce_text in content:
            matches.append(use)
    require(matches, "Claude did not provide a successful tool read containing the hidden nonce")
    return matches
def _successful_tool_result(results: list[dict[str, object]], use: dict[str, object]) -> dict[str, object]:
    matches = [value for value in results if value.get("tool_use_id") == use["id"]]
    require(len(matches) == 1, "Claude tool use has no unique result")
    result = matches[0]
    use_index, result_index = use.get("event_index"), result.get("event_index")
    require(use.get("event_type") == "assistant" and result.get("event_type") == "user" and
            type(use_index) is int and type(result_index) is int and use_index < result_index,
            "Claude tool result lacks a preceding assistant tool use")
    require(result.get("is_error") is False, "Claude positive tool failed")
    return result
def _tool_input_mentions_path(use: dict[str, object], path: Path) -> bool:
    inputs = use.get("input")
    if not (isinstance(inputs, dict)):
        raise AcceptanceFailure("Claude nonce tool input is malformed")
    try:
        encoded = json.dumps(inputs, ensure_ascii=True, sort_keys=True, separators=(",", ":"))
    except (TypeError, ValueError) as error:
        raise AcceptanceFailure("Claude nonce tool input is not encodable") from error
    return str(path) in encoded
def decode_event(line: bytes) -> dict[str, object]:
    require(line and line.strip(), "blank Claude JSONL event line")
    require(len(line) <= MAX_CONTROL_BYTES, "Claude JSONL event line exceeds 1 MiB")
    try:
        value = json.loads(line.decode("utf-8"), object_pairs_hook=unique_object,
                           parse_constant=reject_constant, parse_float=Decimal)
    except (UnicodeError, ValueError, TypeError, RecursionError) as error:
        raise AcceptanceFailure(f"invalid Claude JSONL event: {error}") from error
    if not (isinstance(value, dict)):
        raise AcceptanceFailure("Claude JSONL event is not an object")
    event_type = value.get("type")
    if not (isinstance(event_type, str) and event_type):
        raise AcceptanceFailure("Claude JSONL event type is missing")
    return value
@dataclass
class _StreamEvidence:
    """Own the ordered evidence accumulated from one Claude print stream."""

    init: dict[str, object] | None = None
    result: dict[str, object] | None = None
    events: list[dict[str, object]] = field(default_factory=list)
    tool_uses: list[dict[str, object]] = field(default_factory=list)
    tool_results: list[dict[str, object]] = field(default_factory=list)
    permission_denials: list[dict[str, object]] = field(default_factory=list)
    session_id: str | None = None
    result_seen: bool = False

    def consume(self, event: dict[str, object], index: int) -> None:
        event_type = event["type"]
        if self.result_seen and event_type in {"assistant", "user", "result"}:
            raise AcceptanceFailure("Claude emitted substantive output after terminal result")
        self._reject_terminal_or_unknown_lifecycle(event_type)
        self._bind_event_session(event.get("session_id"))
        if event_type == "system":
            self._consume_system(event)
        elif event_type in {"assistant", "user"}:
            self._consume_message(event, index)
        elif event_type == "result":
            self._consume_result(event)
        else:
            # Unknown non-lifecycle events are telemetry, never answer evidence.
            self.events.append({"type": event_type})

    @staticmethod
    def _reject_terminal_or_unknown_lifecycle(event_type: object) -> None:
        if event_type == "error":
            raise AcceptanceFailure("top-level Claude error event is terminally ambiguous")
        if isinstance(event_type, str) and event_type.startswith(("thread.", "turn.", "session.")):
            raise AcceptanceFailure(
                "unknown Claude thread/turn/session lifecycle event: " + event_type)

    def _bind_event_session(self, session: object) -> None:
        if session is None:
            return
        if not (is_session_id(session)):
            raise AcceptanceFailure("Claude event session_id is not a UUID")
        if self.session_id is None:
            self.session_id = session
        require(self.session_id == session, "Claude event session identity changed")

    def _consume_system(self, event: dict[str, object]) -> None:
        subtype = event.get("subtype")
        if subtype is not None:
            if not (isinstance(subtype, str)):
                raise AcceptanceFailure("Claude system subtype is not a string")
        if subtype == "init":
            require(self.init is None, "multiple Claude init events")
            self.init = event
            init_session = event.get("session_id")
            if not (is_session_id(init_session)):
                raise AcceptanceFailure("Claude init session_id is invalid")
            if self.session_id is None:
                self.session_id = init_session
            require(self.session_id == init_session, "Claude init session identity conflicts")
        elif isinstance(subtype, str) and _is_terminal_system_subtype(subtype):
            raise AcceptanceFailure("unknown terminal Claude system lifecycle event: " + subtype)
        self.events.append(event)

    def _consume_message(self, event: dict[str, object], index: int) -> None:
        require(self.init is not None, "Claude assistant/user event precedes system/init")
        for block in _content_blocks(event):
            if block["type"] == "tool_use":
                self.tool_uses.append({
                    "id": block["id"], "name": block["name"],
                    "input": deepcopy(block["input"]), "event_type": event["type"],
                    "event_index": index,
                })
            elif block["type"] == "tool_result":
                self.tool_results.append({
                    "tool_use_id": block["tool_use_id"],
                    "is_error": block.get("is_error", False),
                    "content": deepcopy(block.get("content", "")),
                    "event_type": event["type"], "event_index": index,
                })
        self.events.append(event)

    def _consume_result(self, event: dict[str, object]) -> None:
        require(self.result is None, "multiple Claude result events")
        require(self.init is not None, "Claude result precedes system/init")
        self.result = event
        self.result_seen = True
        result_session = event.get("session_id")
        if not (is_session_id(result_session)):
            raise AcceptanceFailure("Claude result session_id is invalid")
        if self.session_id is None:
            self.session_id = result_session
        require(self.session_id == result_session, "Claude result session identity conflicts")
        _validate_result_event(event, self.permission_denials)
        self.events.append(event)

    def finish(self) -> ParsedStream:
        init = self.init
        if init is None:
            raise AcceptanceFailure("Claude init event is absent")
        result = self.result
        if result is None:
            raise AcceptanceFailure("Claude terminal result event is absent")
        if not (self.session_id is not None and is_session_id(self.session_id)):
            raise AcceptanceFailure("Claude stream session identity is absent")
        return {
            "session_id": self.session_id,
            "init": init,
            "result_event": result,
            "events": self.events,
            "tool_uses": self.tool_uses,
            "tool_results": self.tool_results,
            "permission_denials": self.permission_denials,
            "usage": result.get("usage"),
            "model_usage": result.get("modelUsage"),
        }


def _is_terminal_system_subtype(subtype: str) -> bool:
    lower = subtype.lower()
    markers = ("abort", "cancel", "error", "fail", "interrupt", "shutdown", "stop",
               "timeout", "timed_out", "timed-out", "refusal")
    return any(marker in lower for marker in markers)


def _validate_result_event(event: dict[str, object],
                           permission_denials: list[dict[str, object]]) -> None:
    subtype = event.get("subtype")
    if not (isinstance(subtype, str)):
        raise AcceptanceFailure("Claude result subtype is missing or not a string")
    require(subtype == "success" or subtype in ERROR_SUBTYPES,
            "unknown Claude result subtype: " + subtype)
    if not (isinstance(event.get("is_error"), bool)):
        raise AcceptanceFailure("Claude result is_error is missing or not boolean")
    require(event["is_error"] is (subtype != "success"),
            "Claude result subtype and is_error disagree")
    if "result" in event:
        if not (isinstance(event["result"], str)):
            raise AcceptanceFailure("Claude result answer is not a string")
    if "stop_reason" in event:
        if not (event["stop_reason"] is None or isinstance(event["stop_reason"], str)):
            raise AcceptanceFailure("Claude stop_reason is not a string or null")
    _validate_result_usage_fields(event)
    denials = event.get("permission_denials")
    if "permission_denials" in event:
        if not (isinstance(denials, list)):
            raise AcceptanceFailure("Claude permission_denials is not an array")
        for denial in denials:
            if not (isinstance(denial, dict)):
                raise AcceptanceFailure("Claude permission denial is not an object")
            permission_denials.append(deepcopy(denial))
    errors = event.get("errors")
    if "errors" in event:
        if not (isinstance(errors, list) and all(isinstance(value, str) for value in errors)):
            raise AcceptanceFailure("Claude result errors are not an array of strings")
        require(subtype != "success" or not errors,
                "Claude successful result has nonempty errors")
    validate_usage_shapes(event)


def _validate_result_usage_fields(event: dict[str, object]) -> None:
    for field_name in ("usage", "modelUsage"):
        if field_name in event:
            if not (event[field_name] is None or isinstance(event[field_name], dict)):
                raise AcceptanceFailure("Claude " + field_name + " is not an object or null")


def parse_claude_events(raw: bytes) -> ParsedStream:
    """Parse the bounded Claude print stream without selecting prose as success."""
    require(len(raw) <= MAX_PROVIDER_BYTES, "Claude stdout exceeds observation bound")
    lines = raw.split(b"\n")
    if lines and lines[-1] == b"":
        lines.pop()
    require(lines, "Claude JSONL stdout is empty")
    stream = _StreamEvidence()
    for index, raw_line in enumerate(lines):
        stream.consume(decode_event(raw_line), index)
    return stream.finish()
def reject_constant(value: str) -> None:
    raise ValueError("nonfinite JSON number: " + value)
def validate_claude_result(parsed: Mapping[str, object], expected_session: str | None = None,
                           expected_answer: bytes | None = None) -> bytes:
    """Require Claude's terminal result predicate before accepting bytes."""
    session = parsed.get("session_id")
    if not (is_session_id(session)):
        raise AcceptanceFailure("Claude result has no valid session identity")
    if expected_session is not None:
        require(session == expected_session, "Claude result session identity mismatch")
    result = parsed.get("result_event")
    if not (isinstance(result, dict)):
        raise AcceptanceFailure("Claude result event is absent")
    subtype = result.get("subtype")
    require(subtype == "success", "Claude result is not a successful subtype")
    require(result.get("is_error") is False, "Claude successful result is_error is not false")
    answer = result.get("result")
    if not (isinstance(answer, str) and answer.strip()):
        raise AcceptanceFailure("Claude result answer is blank or absent")
    stop_reason = result.get("stop_reason")
    if stop_reason is not None:
        require(stop_reason.lower() in {"end_turn", "stop_sequence"},
                "Claude result has an incomplete/refused/unknown stop reason")
    require(subtype not in ERROR_SUBTYPES, "Claude result has an error subtype")
    data = answer.encode("utf-8")
    if expected_answer is not None:
        require(data == expected_answer, "Claude result bytes differ from expected answer")
    return data
def validate_fresh_controls(parsed: Mapping[str, object], nonce_file: Path,
                            workspace: Path, sibling: Path, nonce: bytes,
                            expected_session: str | None = None) -> dict[str, object]:
    """Require real read/search calls and preserve native policy evidence."""
    init = validate_init_profile(parsed, expected_session=expected_session,
                                 expected_cwd=str(workspace))
    validate_claude_result(parsed, expected_session=str(init["session_id"]),
                           expected_answer=nonce)
    uses = parsed.get("tool_uses")
    results = parsed.get("tool_results")
    if not (isinstance(uses, list) and isinstance(results, list)):
        raise AcceptanceFailure("Claude tool evidence is absent")
    for use in uses:
        if not (isinstance(use, dict)):
            raise AcceptanceFailure("Claude tool use evidence is malformed")
        if not (isinstance(use.get("name"), str) and use["name"]):
            raise AcceptanceFailure("Claude tool name evidence is malformed")
    require(len({use["id"] for use in uses}) == len(uses), "Claude tool use IDs are not unique")
    nonce_uses = _nonce_tool_uses(results, uses, nonce_file, nonce)
    denials = parsed.get("permission_denials")
    if not (isinstance(denials, list)):
        raise AcceptanceFailure("Claude permission-denial evidence is malformed")
    return {
        "session_id": init["session_id"],
        "tool_names": list(init["tools"]),
        "api_key_source": init.get("api_key_source"),
        "mcp_server_count": init["mcp_server_count"],
        "nonce_read": True,
        "nonce_tool_name": nonce_uses[0]["name"],
        "nonce_read_count": len(nonce_uses),
        "permission_denial_count": len(denials),
        "tool_use_count": len(uses),
    }
def validate_init_profile(parsed: Mapping[str, object], expected_session: str | None = None,
                          expected_cwd: str | None = None) -> InitProfile:
    """Bind identity and requested permission while leaving policy to Claude."""
    init = parsed.get("init")
    if not (isinstance(init, dict)):
        raise AcceptanceFailure("Claude init evidence is absent")
    session = init.get("session_id")
    if not (is_session_id(session)):
        raise AcceptanceFailure("Claude init session identity is invalid")
    if expected_session is not None:
        require(session == expected_session, "Claude init session identity mismatch")
    cwd = init.get("cwd")
    if not (isinstance(cwd, str) and cwd and "\x00" not in cwd):
        raise AcceptanceFailure("Claude init cwd is absent or malformed")
    if expected_cwd is not None:
        require(cwd == expected_cwd, "Claude init cwd differs from the acceptance workspace")
    tools = init.get("tools")
    if not (isinstance(tools, list)):
        raise AcceptanceFailure("Claude init tool list is absent or malformed")
    if not (all(isinstance(tool, str) and tool for tool in tools)):
        raise AcceptanceFailure("Claude init tool list is malformed")
    require(len(set(tools)) == len(tools), "Claude init tool list is duplicated")
    permission = init.get("permissionMode")
    if not isinstance(permission, str) or permission != APPROVAL:
        raise AcceptanceFailure("Claude init permission mode is not plan")
    if "model" in init:
        model = init["model"]
        if not (isinstance(model, str) and model.strip() == model and model and "\x00" not in model):
            raise AcceptanceFailure("Claude init model is malformed")
    api_key_source = init.get("apiKeySource")
    if not (isinstance(api_key_source, str) and api_key_source.strip() == api_key_source and api_key_source):
        raise AcceptanceFailure("Claude init apiKeySource is missing or malformed")
    mcp_servers = init.get("mcp_servers")
    if not (isinstance(mcp_servers, list)):
        raise AcceptanceFailure("Claude init mcp_servers are not an array")
    return {"session_id": session, "tools": list(tools), "permission_mode": permission,
            "api_key_source": api_key_source, "mcp_server_count": len(mcp_servers)}
def validate_tool_free_resume(parsed: Mapping[str, object], expected_answer: bytes,
                              expected_session: str | None = None) -> dict[str, object]:
    """Accept a continuation only when no Claude tool item/event occurred."""
    init = validate_init_profile(parsed, expected_session)
    answer = validate_claude_result(parsed, expected_session=str(init["session_id"]))
    require(answer == expected_answer, "Claude resume returned an unexpected continuation answer")
    uses = parsed.get("tool_uses")
    results = parsed.get("tool_results")
    if not (isinstance(uses, list) and not uses):
        raise AcceptanceFailure("Claude resume used a tool")
    if not (isinstance(results, list) and not results):
        raise AcceptanceFailure("Claude resume returned a tool result")
    denials = parsed.get("permission_denials")
    if not (isinstance(denials, list) and not denials):
        raise AcceptanceFailure("Claude resume contains permission-denial evidence")
    item_types: list[str] = []
    item_count = 0
    events = parsed.get("events", [])
    if not isinstance(events, list):
        raise AcceptanceFailure("Claude resume event evidence is malformed")
    for event in events:
        if not isinstance(event, dict) or event.get("type") not in {"assistant", "user"}:
            continue
        blocks = _content_blocks(event)
        for block in blocks:
            kind = block["type"]
            if not isinstance(kind, str) or kind not in {"text", "thinking", "redacted_thinking"}:
                raise AcceptanceFailure("Claude resume contains a disallowed item type")
            item_count += 1
            if kind not in item_types:
                item_types.append(kind)
    return {"tool_free": True, "item_count": item_count, "item_types": item_types,
            "session_id": init["session_id"]}
def validate_usage_shapes(result: dict[str, object]) -> None:
    """Bound counters while preserving missing values and independent views."""
    usage = result.get("usage")
    if usage is not None:
        if not (isinstance(usage, dict)):
            raise AcceptanceFailure("Claude result usage is not an object")
        for field in ("input_tokens", "output_tokens", "thinking_tokens", "total_tokens",
                      "cache_read_input_tokens", "cache_creation_input_tokens"):
            _counter(usage.get(field), "usage." + field)
    models = result.get("modelUsage")
    if models is not None:
        if not (isinstance(models, dict) and len(models) <= 128):
            raise AcceptanceFailure("Claude result modelUsage exceeds its bounded record count")
        for model, fields in models.items():
            if not (isinstance(model, str) and model.strip() == model and model):
                raise AcceptanceFailure("Claude result modelUsage has an invalid model name")
            if not (isinstance(fields, dict)):
                raise AcceptanceFailure("Claude result modelUsage entry is not an object")
            for field in ("inputTokens", "outputTokens", "thinkingTokens", "totalTokens",
                          "cacheReadInputTokens", "cacheCreationInputTokens"):
                _counter(fields.get(field), "modelUsage." + field)
            _decimal(fields.get("costUSD"), "modelUsage.costUSD")
    _decimal(result.get("total_cost_usd"), "result.total_cost_usd")
    for field in ("duration_ms", "num_turns"):
        _counter(result.get(field), "result." + field)
