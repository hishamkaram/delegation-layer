"""Codex JSONL lifecycle and resume predicates."""

from __future__ import annotations

from dataclasses import dataclass, field
import json

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.evidence.json_decoder import unique_object
from scripts.delegate_acceptance.provider_drivers.codex.constants import (
    MAX_CONTROL_BYTES, MAX_INT64, MAX_ITEM_ID_BYTES, MAX_ITEM_TYPE_BYTES,
    MAX_ITEMS, MAX_PROVIDER_BYTES, RESUME_ALLOWED_ITEM_TYPES,
)
from scripts.delegate_acceptance.provider_drivers.codex.identity import is_thread_id

def reject_constant(value: str) -> None:
    raise ValueError("nonfinite JSON number: " + value)


def decode_event(line: bytes) -> dict[str, object]:
    require(line and line.strip(), "blank Codex JSONL event line")
    require(len(line) <= MAX_CONTROL_BYTES, "Codex JSONL event line exceeds 1 MiB")
    try:
        text = line.decode("utf-8")
        value = json.loads(text, object_pairs_hook=unique_object, parse_constant=reject_constant)
    except (UnicodeError, ValueError, TypeError, RecursionError) as error:
        raise AcceptanceFailure(f"invalid Codex JSONL event: {error}") from error
    require(isinstance(value, dict), "Codex JSONL event is not an object")
    require(isinstance(value.get("type"), str) and value["type"],
            "Codex JSONL event type is missing")
    return value


def nonnegative_counter(value: object, label: str) -> int | None:
    if value is None:
        return None
    require(isinstance(value, int) and not isinstance(value, bool) and 0 <= value <= MAX_INT64,
            f"{label} is not a nonnegative integer")
    return value


def parse_usage(event: dict[str, object]) -> dict[str, int | None] | None:
    usage = event.get("usage")
    if usage is None:
        return None
    require(isinstance(usage, dict), "turn.completed usage is not an object")
    return {
        "input_tokens": nonnegative_counter(usage.get("input_tokens"), "usage input_tokens"),
        "cached_input_tokens": nonnegative_counter(usage.get("cached_input_tokens"), "usage cached_input_tokens"),
        "cache_write_input_tokens": nonnegative_counter(usage.get("cache_write_input_tokens"), "usage cache_write_input_tokens"),
        "output_tokens": nonnegative_counter(usage.get("output_tokens"), "usage output_tokens"),
        "reasoning_output_tokens": nonnegative_counter(usage.get("reasoning_output_tokens"), "usage reasoning_output_tokens"),
    }


@dataclass
class _CodexStream:
    """Own one ordered Codex turn and the evidence accumulated from it."""

    thread_id: str | None = None
    turn_started: bool = False
    turn_completed: bool = False
    terminal_failed: bool = False
    interrupted: bool = False
    semantic_error: str | None = None
    final_message: bytes | None = None
    usage: dict[str, int | None] | None = None
    items: dict[str, dict[str, object]] = field(default_factory=dict)
    commands: dict[str, dict[str, object]] = field(default_factory=dict)
    unknown_events: list[str] = field(default_factory=list)

    def fault(self, message: str) -> None:
        if self.semantic_error is None:
            self.semantic_error = message

    def consume(self, event: dict[str, object]) -> None:
        event_type = event["type"]
        if event_type == "thread.started":
            self._thread_started(event)
        elif event_type == "turn.started":
            self._turn_started()
        elif event_type == "turn.completed":
            self._turn_completed(event)
        elif event_type == "turn.failed":
            self._turn_failed(event)
        elif event_type in {"turn.interrupted", "turn.cancelled", "turn.aborted"}:
            self._interrupt()
        elif event_type == "error":
            self._error(event)
        elif event_type in {"item.started", "item.updated", "item.completed"}:
            self._item_event(event_type, event)
        else:
            self._unknown_event(event_type)

    def _thread_started(self, event: dict[str, object]) -> None:
        if self.thread_id is not None:
            self.fault("multiple thread.started events")
            return
        value = event.get("thread_id")
        if not is_thread_id(value):
            self.fault("thread.started thread_id is not a UUID")
            return
        self.thread_id = value

    def _turn_started(self) -> None:
        if self.thread_id is None:
            self.fault("turn.started precedes thread.started")
        elif self.turn_started or self.turn_completed or self.terminal_failed or self.interrupted:
            self.fault("duplicate or late turn.started event")
        else:
            self.turn_started = True

    def _turn_completed(self, event: dict[str, object]) -> None:
        if self.thread_id is None or not self.turn_started:
            self.fault("turn.completed has no active turn")
        elif self.turn_completed or self.terminal_failed or self.interrupted:
            self.fault("conflicting turn terminal events")
        else:
            status = event.get("status")
            if status is not None and not isinstance(status, str):
                self.fault("turn.completed status is not a string")
            elif status not in (None, "", "completed"):
                self.interrupted = True
            else:
                try:
                    self.usage = parse_usage(event)
                except AcceptanceFailure as error:
                    self.fault(str(error))
                else:
                    self.turn_completed = True

    def _turn_failed(self, event: dict[str, object]) -> None:
        if (self.thread_id is None or not self.turn_started or self.turn_completed or
                self.terminal_failed or self.interrupted):
            self.fault("turn.failed has no active turn")
            return
        failure = event.get("error")
        if not isinstance(failure, dict):
            self.fault("turn.failed error is not an object")
        elif not isinstance(failure.get("message"), str):
            self.fault("turn.failed error message is not a string")
        else:
            self.terminal_failed = True

    def _interrupt(self) -> None:
        if (self.thread_id is None or not self.turn_started or self.turn_completed or
                self.terminal_failed or self.interrupted):
            self.fault("terminal interruption follows turn completion")
        else:
            self.interrupted = True

    def _error(self, event: dict[str, object]) -> None:
        if not isinstance(event.get("message"), str):
            self.fault("error message is not a string")
        elif self.turn_completed:
            self.fault("error event follows turn completion")

    def _item_event(self, event_type: str, event: dict[str, object]) -> None:
        prepared = self._validated_item(event)
        if prepared is None:
            return
        item, item_id, item_type, text = prepared
        if self._is_preturn_error(event_type, item_type):
            return
        if not self._item_is_in_active_turn():
            self.fault("item event is outside the active turn")
            return
        if item_id not in self.items and len(self.items) >= MAX_ITEMS:
            self.fault("item identity state exceeds bounded count")
            return
        state = self.items.setdefault(item_id, {
            "type": item_type, "started": False, "updated": False, "completed": False,
        })
        if state["type"] != item_type:
            self.fault("item id changed type")
            return
        self._record_item_payload(item, item_id, item_type, text, state)
        self._advance_item(event_type, item_type, state)

    def _validated_item(self, event: dict[str, object]) -> tuple[
            dict[str, object], str, str, str | None] | None:
        item = event.get("item")
        if not isinstance(item, dict):
            self.fault("item event has no object item")
            return None
        item_id = item.get("id")
        item_type = item.get("type")
        if not isinstance(item_id, str) or not item_id:
            self.fault("item id is not a nonempty string")
            return None
        if not isinstance(item_type, str) or not item_type:
            self.fault("item type is not a nonempty string")
            return None
        if len(item_id.encode("utf-8")) > MAX_ITEM_ID_BYTES:
            self.fault("item id exceeds bounded size")
            return None
        if len(item_type.encode("utf-8")) > MAX_ITEM_TYPE_BYTES:
            self.fault("item type exceeds bounded size")
            return None
        text = item.get("text") if item_type == "agent_message" else None
        if item_type == "agent_message" and not isinstance(text, str):
            self.fault("agent_message text is not a string")
            return None
        if item_type == "error" and not isinstance(item.get("message"), str):
            self.fault("error item message is not a string")
            return None
        return item, item_id, item_type, text if isinstance(text, str) else None

    def _is_preturn_error(self, event_type: str, item_type: str) -> bool:
        # Codex may publish a completed error item as a configuration
        # diagnostic after thread.started and before turn.started. It is
        # outside the turn lifecycle and must not enter item state, while its
        # identity and message remain subject to the same validation as Go.
        return (self.thread_id is not None and not self.turn_started and
                event_type == "item.completed" and item_type == "error")

    def _item_is_in_active_turn(self) -> bool:
        return (self.thread_id is not None and self.turn_started and not self.turn_completed and
                not self.terminal_failed and not self.interrupted)

    def _record_item_payload(self, item: dict[str, object], item_id: str, item_type: str,
                             text: str | None, state: dict[str, object]) -> None:
        if item_type == "agent_message":
            state["text"] = text
        elif item_type == "command_execution":
            command = self.commands.setdefault(item_id, {"id": item_id, "type": item_type})
            for key in ("command", "aggregated_output", "status", "exit_code"):
                if key in item:
                    command[key] = item[key]

    def _advance_item(self, event_type: str, item_type: str,
                      state: dict[str, object]) -> None:
        if event_type == "item.started":
            if state["started"] or state["completed"]:
                self.fault("duplicate item.started event")
            else:
                state["started"] = True
        elif event_type == "item.updated":
            if not state["started"]:
                self.fault("item.updated precedes item.started")
            elif state["completed"]:
                self.fault("item.updated follows completion")
            else:
                state["updated"] = True
        elif state["completed"]:
            self.fault("duplicate item.completed event")
        else:
            state["completed"] = True
            if item_type == "agent_message":
                self.final_message = state.get("text", "").encode("utf-8")

    def _unknown_event(self, event_type: str) -> None:
        # Unknown nonterminal events remain tolerated unless they alter turn identity.
        if event_type.startswith(("thread.", "turn.")):
            self.fault("unknown thread/turn lifecycle event: " + event_type)
        else:
            self.unknown_events.append(event_type)

    def finish(self) -> dict[str, object]:
        require(self.semantic_error is None,
                self.semantic_error or "invalid Codex JSONL evidence")
        require(self.thread_id is not None, "Codex JSONL has no thread.started identity")
        require(self.turn_started and self.turn_completed and
                not self.terminal_failed and not self.interrupted,
                "Codex JSONL has no successful completed turn")
        require(self.final_message is not None and self.final_message.strip(),
                "Codex JSONL has no non-whitespace final agent message")
        return self._result()

    def _result(self) -> dict[str, object]:
        # Only item.completed commands prove provider action; earlier command
        # events can contain a plan without showing that it was attempted.
        completed_commands = [
            command for item_id, command in self.commands.items()
            if self.items.get(item_id, {}).get("completed") is True
        ]
        incomplete_commands = [
            item_id for item_id, state in self.items.items()
            if state.get("type") == "command_execution" and state.get("completed") is not True
        ]
        item_states = [
            {"id": item_id, "type": state["type"], "started": state["started"],
             "updated": state["updated"], "completed": state["completed"]}
            for item_id, state in self.items.items()
        ]
        return {
            "thread_id": self.thread_id,
            "final_message": self.final_message,
            "usage": self.usage,
            "commands": completed_commands,
            "incomplete_commands": incomplete_commands,
            "item_states": item_states,
            "unknown_events": self.unknown_events,
        }


def parse_codex_events(raw: bytes) -> dict[str, object]:
    """Apply the immutable Codex JSONL selection and completion rules."""
    require(len(raw) <= MAX_PROVIDER_BYTES, "Codex stdout exceeds observation bound")
    lines = raw.split(b"\n")
    if lines and lines[-1] == b"":
        lines.pop()
    require(lines, "Codex JSONL stdout is empty")
    stream = _CodexStream()
    for raw_line in lines:
        try:
            event = decode_event(raw_line)
        except AcceptanceFailure as error:
            stream.fault(str(error))
            continue
        stream.consume(event)
    return stream.finish()


def validate_tool_free_resume(parsed: dict[str, object], expected_answer: bytes) -> dict[str, object]:
    """Require a resumed turn to contain only conversational item kinds."""
    require(isinstance(expected_answer, bytes), "resume expected answer is not bytes")
    item_states = parsed.get("item_states")
    require(isinstance(item_states, list), "resume item type evidence is absent")
    unknown_events = parsed.get("unknown_events")
    require(isinstance(unknown_events, list) and not unknown_events,
            "resume contains unknown event types")
    types: list[str] = []
    for state in item_states:
        require(isinstance(state, dict), "resume item type evidence is malformed")
        item_type = state.get("type")
        require(isinstance(item_type, str) and item_type in RESUME_ALLOWED_ITEM_TYPES,
                "resume contains disallowed item type: " + str(item_type))
        types.append(item_type)
    commands = parsed.get("commands")
    require(isinstance(commands, list) and not commands,
            "resume contains completed command_execution evidence")
    require(parsed.get("final_message") == expected_answer,
            "resume final message is not the exact recovered nonce")
    return {"item_count": len(item_states), "item_types": sorted(set(types)),
            "tool_free": True}
