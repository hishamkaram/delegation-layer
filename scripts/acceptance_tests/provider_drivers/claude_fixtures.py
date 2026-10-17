"""Shared Claude stream fixtures for acceptance-driver tests."""

from __future__ import annotations

import json
from pathlib import Path


SESSION = "0199a213-81c0-7800-8aa1-bbab2a035a53"
NONCE = b"0123456789abcdef0123456789abcdef0123456789abcdef"


def event(value: dict[str, object]) -> bytes:
    return json.dumps(value, ensure_ascii=True, separators=(",", ":")).encode()


def init_event(session: str = SESSION, tools: list[str] | None = None,
               permission: str = "plan") -> dict[str, object]:
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
                 denied_tools: tuple[str, ...] = (), tool_name: str = "Bash",
                 tool_input: dict[str, object] | None = None, tool_output: str | None = None,
                 repeated_reads: int = 0) -> bytes:
    values: list[dict[str, object]] = [init_event(tools=[tool_name])]
    values[0]["cwd"] = str(workspace)
    inputs = tool_input or {"command": f"cat -- {nonce_file}"}
    output = tool_output or nonce.decode() + "\n"
    for index in range(repeated_reads + 1):
        identifier = "nonce" if index == 0 else f"nonce-{index}"
        values.extend([tool_use(identifier, tool_name, **inputs), tool_result(identifier, output)])
    values.append({"type": "assistant", "session_id": SESSION,
                   "message": {"role": "assistant", "content": [{"type": "text", "text": nonce.decode()}]}})
    result = answer_event(nonce)
    result["permission_denials"] = [permission_denial(name) for name in sorted(denied_tools)]
    values.append(result)
    return stream(values)


def bash_plain_stream(nonce_file: Path, workspace: Path, nonce: bytes = NONCE) -> bytes:
    return fresh_stream(nonce_file, workspace, nonce, tool_name="Bash",
                        tool_input={"command": f"cat -- {nonce_file}"},
                        tool_output=nonce.decode() + "\n")


def native_numbered_read_stream(nonce_file: Path, workspace: Path, nonce: bytes = NONCE) -> bytes:
    return fresh_stream(nonce_file, workspace, nonce, tool_name="Read",
                        tool_input={"file_path": str(nonce_file)},
                        tool_output=f"     1→{nonce.decode()}\n", repeated_reads=1)
