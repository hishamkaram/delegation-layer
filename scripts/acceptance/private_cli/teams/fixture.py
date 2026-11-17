"""Nonce-bound Pi provider fixture; performs only the owned Team proof writes."""

import json
from pathlib import Path
import re
import subprocess


def main(prompt: str, nonce: str, arguments: list[str], *, emit_session: bool = True):
    """Require the exact write capability and nonce before creating any artifact."""
    if (re.fullmatch(r"[0-9a-f]{32}", nonce) is None
            or prompt.splitlines().count("DELEGATE_TEAMS_WRITE:" + nonce) != 1):
        raise ValueError("Team write fixture requires its exact owned nonce")
    for flag, value in (("--mode", "json"), ("--tools", "read,bash,edit,write"),
                        ("--model", "openai/gpt-4.1"), ("--thinking", "low")):
        if arguments.count(flag) != 1:
            raise ValueError("Team write fixture requires explicit " + flag)
        position = arguments.index(flag)
        if position + 1 >= len(arguments) or arguments[position + 1] != value:
            raise ValueError("Team write fixture received unexpected " + flag)

    artifact = Path("team-write-" + nonce + ".txt")
    with artifact.open("x", encoding="utf-8") as stream:
        stream.write("created:" + nonce + "\n")
    with artifact.open("r+", encoding="utf-8") as stream:
        original = stream.read()
        stream.seek(0)
        stream.write(original + "edited:" + nonce + "\n")
        stream.truncate()
    result = subprocess.run(
        ["/bin/sh", "-c", 'printf "command:%s\\n" "$1"', "team-write-proof", nonce],
        check=True, capture_output=True, timeout=5,
    )
    with Path("team-command-" + nonce + ".txt").open("xb") as stream:
        stream.write(result.stdout)

    message = {"role": "assistant", "content": [{"type": "text", "text": "team workspace-write accepted " + nonce}],
               "stopReason": "stop"}
    if emit_session:
        print(json.dumps({"type": "session", "id": "00000000-0000-4000-8000-000000000001"}))
    for event in ({"type": "agent_start"}, {"type": "turn_start"},
                  {"type": "message_start", "message": {"role": "assistant", "content": []}},
                  {"type": "message_end", "message": message},
                  {"type": "turn_end", "message": message, "toolResults": []},
                  {"type": "agent_end", "messages": [message]}):
        print(json.dumps(event))
