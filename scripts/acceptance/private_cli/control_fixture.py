"""Finite executable client gate; importing this module starts no fixture.

The launch mode gives one CLI an owned TMPDIR. Native defaultEnvironmentKey
preserves that explicit temporary-directory port through its control owner.
Only the exact root's JSON status call enters the gate; version/help pass through.
"""

import json
import os
from pathlib import Path
import sys
import time


FIXTURE_WAIT_SECONDS = 20


def wait_for_release(path: Path, seconds: float = FIXTURE_WAIT_SECONDS) -> None:
    deadline = time.monotonic() + seconds
    while not path.is_file():
        if time.monotonic() >= deadline:
            raise RuntimeError("finite control fixture release deadline expired")
        time.sleep(0.01)


def publish_event(path: Path, record: dict[str, object]) -> None:
    """Publish a complete ephemeral fixture event without exposing partial JSON."""
    stage = path.with_name(path.name + "." + str(os.getpid()) + ".staging")
    try:
        with stage.open("x", encoding="utf-8") as stream:
            json.dump(record, stream, sort_keys=True)
        os.link(stage, path)
    finally:
        stage.unlink(missing_ok=True)


def run_client(executable: str, controls: Path, arguments: list[str]) -> None:
    temporary = os.environ.get("TMPDIR")
    port = Path(temporary) if temporary else None
    if port is not None and port.parent == controls:
        request = json.loads((port / "request.json").read_text())
        matches = (
            len(arguments) >= 4
            and arguments[0] in {"-c", "--config"}
            and arguments[1] == request["config"]
            and arguments[2] == "status"
            and "--json" in arguments[3:]
        )
        if matches:
            launched = json.loads((port / "launched.json").read_text())
            if launched["nonce"] != request["nonce"]:
                raise RuntimeError("finite client gate launch nonce changed")
            receipt = port / "entered.json"
            if not receipt.exists():
                publish_event(
                    receipt,
                    {
                        "schema_version": 1,
                        "nonce": request["nonce"],
                        "cli_pid": launched["cli_pid"],
                        "control_owner_pid": os.getppid(),
                        "client_pid": os.getpid(),
                        "entered_ns": time.monotonic_ns(),
                        "command": "status-json",
                        "arguments": arguments[2:],
                        "config": arguments[1],
                    },
                )
            wait_for_release(port / "release")
    os.execv(executable, [executable, *arguments])


def main() -> None:
    operation, executable, control, *arguments = sys.argv[1:]
    port = Path(control)
    if operation == "launch":
        wait_for_release(port / "launch")
        request = json.loads((port / "request.json").read_text())
        publish_event(port / "launched.json", {"schema_version": 1, "cli_pid": os.getpid(), "nonce": request["nonce"]})
        os.environ["TMPDIR"] = str(port)
        os.execv(executable, [executable, *arguments])
    elif operation == "client":
        run_client(executable, port, arguments)
    else:
        raise RuntimeError("unknown finite control fixture operation")


if __name__ == "__main__":
    main()
