"""Codex immutable shell probe construction and observation."""

from __future__ import annotations

import os
import re
import shlex
from pathlib import Path

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.paths.operations import path_is_within
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.provider_drivers.codex.constants import (
    DENIAL_WORDS, MAX_CONTROL_BYTES, PROBE_MARKERS,
    RECOGNIZED_SHELL_WRAPPERS, REJECTED_SHELL_SYNTAX,
)

def shell_tokens(command: str) -> list[str] | None:
    """Tokenize one shell command while preserving operator boundaries."""
    try:
        lexer = shlex.shlex(command, posix=True, punctuation_chars="><;&|")
        lexer.whitespace_split = True
        return list(lexer)
    except ValueError:
        return None


def unwrap_shell_command(command: str) -> list[str] | None:
    """Unwrap one finite, recognized absolute-shell ``-c`` or ``-lc`` invocation.

    Codex may ask its inherited shell to run a command.  Only an exact
    ``/bin/<shell> (-c|-lc) <one string>`` shape is unwrapped; shell operators,
    additional arguments, and nested wrappers remain visible to the caller.
    """
    if any(character in command for character in REJECTED_SHELL_SYNTAX):
        return None
    tokens = shell_tokens(command)
    if tokens is None:
        return None
    if len(tokens) == 2:
        return tokens
    if len(tokens) != 3 or tokens[0] not in RECOGNIZED_SHELL_WRAPPERS or tokens[1] not in {"-c", "-lc"}:
        return tokens
    return shell_tokens(tokens[2])


def command_reads_path(command: str, path: Path) -> bool:
    """Recognize one exact ``cat`` read, including the native shell wrapper."""
    tokens = unwrap_shell_command(command)
    if tokens is None or not tokens:
        return False
    executable = tokens[0]
    if executable != "cat" and (not executable.startswith("/") or Path(executable).name != "cat"):
        return False
    target = str(path)
    return tokens[1:] in ([target], ["--", target])


def exact_probe_command(command: str, probe: Path) -> bool:
    """Match only the native probe command after one recognized shell wrapper."""
    target = str(probe)
    if not probe.is_absolute() or probe != Path(os.path.normpath(target)):
        return False
    tokens = unwrap_shell_command(command)
    return tokens == ["/bin/sh", target]


def probe_script_bytes(nonce_file: Path, inside: Path, sibling: Path) -> bytes:
    """Build the immutable shell probe from paths only.

    The probe reads the nonce and then performs both writes in independent
    grouped substitutions.  No nonce value is embedded in the script, so the
    script itself can be safely bound by a digest in the acceptance receipt.
    """
    paths = (nonce_file, inside, sibling)
    for path in paths:
        value = str(path)
        require(path.is_absolute() and path == Path(os.path.normpath(value)),
                "probe paths must be absolute and clean")
        require("\x00" not in value and "\r" not in value and "\n" not in value,
                "probe paths cannot contain shell line breaks")
    def quote_path(path: Path) -> str:
        quoted = shlex.quote(str(path))
        return quoted if quoted.startswith("'") else "'" + quoted + "'"

    nonce_path, inside_path, sibling_path = (quote_path(path) for path in paths)
    return (
        "#!/bin/sh\n"
        "set +e\n"
        f"nonce_value=$(/bin/cat {nonce_path})\n"
        "read_rc=$?\n"
        "printf 'probe.v1.read_rc=%s\\n' \"$read_rc\"\n"
        "printf 'probe.v1.nonce=%s\\n' \"$nonce_value\"\n"
        f"workspace_error=$( ( printf 'WORKSPACE-WRITE-PROBE\\n' > {inside_path} ) 2>&1 )\n"
        "workspace_rc=$?\n"
        "printf 'probe.v1.workspace_rc=%s\\n' \"$workspace_rc\"\n"
        "printf 'probe.v1.workspace_stderr=%s\\n' \"$workspace_error\"\n"
        f"sibling_error=$( ( printf 'SIBLING-WRITE-PROBE\\n' > {sibling_path} ) 2>&1 )\n"
        "sibling_rc=$?\n"
        "printf 'probe.v1.sibling_rc=%s\\n' \"$sibling_rc\"\n"
        "printf 'probe.v1.sibling_stderr=%s\\n' \"$sibling_error\"\n"
        "overall=0\n"
        "if [ \"$read_rc\" -ne 0 ]; then overall=1; fi\n"
        "if [ \"$workspace_rc\" -eq 0 ]; then overall=1; fi\n"
        "if [ \"$sibling_rc\" -eq 0 ]; then overall=1; fi\n"
        "exit \"$overall\"\n"
    ).encode()


def parse_probe_markers(output: str, nonce: bytes, inside: Path, sibling: Path) -> dict[str, object]:
    """Parse the probe's bounded machine markers from one command result."""
    require(isinstance(output, str), "probe command output is not text")
    try:
        output_bytes = output.encode("utf-8")
    except UnicodeEncodeError as error:
        raise AcceptanceFailure("probe command output is not valid UTF-8 text") from error
    require(len(output_bytes) <= MAX_CONTROL_BYTES, "probe command output exceeds the control bound")
    try:
        expected_nonce = nonce.decode("ascii")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure("probe nonce is not ASCII") from error
    markers: dict[str, str] = {}
    for line in output.splitlines():
        if not line.startswith("probe.v1."):
            continue
        key, separator, value = line.partition("=")
        require(separator == "=" and key in PROBE_MARKERS,
                "probe output contains an unknown marker")
        require(key not in markers, "probe output contains a duplicate marker: " + key)
        require("\n" not in value and "\r" not in value,
                "probe marker contains a line break")
        markers[key] = value
    require(set(markers) == set(PROBE_MARKERS), "probe output markers are incomplete")
    require(markers["probe.v1.read_rc"] == "0", "probe read did not complete with exit code 0")
    require(markers["probe.v1.nonce"] == expected_nonce,
            "probe read marker did not return the exact nonce")

    parsed: dict[str, object] = {name: markers[marker] for marker, name in PROBE_MARKERS.items()
                                 if name != "nonce"}
    parsed["read_rc"] = 0
    for name, path in (("workspace", inside), ("sibling", sibling)):
        rc_name = name + "_rc"
        stderr_name = name + "_stderr"
        value = parsed[rc_name]
        require(isinstance(value, str) and re.fullmatch(r"[0-9]+", value) is not None,
                f"probe {name} exit code is not numeric")
        code = int(value)
        require(0 < code <= 255, f"probe {name} write was not independently denied")
        parsed[rc_name] = code
        diagnostic = parsed[stderr_name]
        require(isinstance(diagnostic, str) and diagnostic,
                f"probe {name} denial diagnostic is empty")
        diagnostic_lower = diagnostic.lower()
        require(str(path) in diagnostic,
                f"probe {name} denial diagnostic names the wrong absolute path")
        require(any(word in diagnostic_lower for word in DENIAL_WORDS),
                f"probe {name} denial diagnostic has no permission failure")
    return parsed


def validate_command_execution_controls(parsed: dict[str, object], probe: Path,
                                        inside: Path, sibling: Path, nonce: bytes) -> dict[str, object]:
    """Require one immutable probe command and actual read/write observations."""
    commands = parsed.get("commands")
    require(isinstance(commands, list) and len(commands) == 1,
            "Codex must produce exactly one completed probe command_execution item")
    incomplete = parsed.get("incomplete_commands", [])
    require(isinstance(incomplete, list) and not incomplete,
            "incomplete command_execution items cannot prove probe controls")
    item = commands[0]
    require(isinstance(item, dict), "probe command_execution item is malformed")
    require(item.get("type") == "command_execution", "probe command_execution item has the wrong type")
    item_id = item.get("id")
    require(isinstance(item_id, str) and item_id, "probe command_execution item has no identity")
    command = item.get("command")
    require(isinstance(command, str) and exact_probe_command(command, probe),
            "command_execution evidence is not the exact immutable /bin/sh probe")
    exit_code = item.get("exit_code")
    require(item.get("status") == "completed" and isinstance(exit_code, int) and
            not isinstance(exit_code, bool) and exit_code == 0,
            "probe command_execution did not complete successfully")
    output = item.get("aggregated_output")
    require(isinstance(output, str), "probe command_execution output is not text")
    markers = parse_probe_markers(output, nonce, inside, sibling)
    return {
        "probe_command_id": item_id,
        "probe_command": command,
        "read_rc": markers["read_rc"],
        "workspace_rc": markers["workspace_rc"],
        "sibling_rc": markers["sibling_rc"],
        "workspace_stderr": markers["workspace_stderr"],
        "sibling_stderr": markers["sibling_stderr"],
    }


def require_probe_file_unchanged(probe: Path, state_parent: Path, state: Path,
                                 workspace: Path, sibling: Path, phase: str,
                                 expected_size: int, expected_digest: str,
                                 nonce: bytes) -> None:
    """Verify the immutable probe remains isolated and byte-identical."""
    require(probe.parent == state_parent and
            not path_is_within(probe, state) and not path_is_within(state, probe) and
            not path_is_within(probe, workspace) and not path_is_within(workspace, probe) and
            not path_is_within(probe, sibling) and not path_is_within(sibling, probe),
            f"{phase}: probe is not separate from state/workspace")
    require(probe.is_file() and not probe.is_symlink(),
            f"{phase}: probe is not a regular non-symlink file")
    require(probe.resolve(strict=True) == probe,
            f"{phase}: probe is not canonical")
    metadata = probe.stat()
    require(metadata.st_mode & 0o7777 == 0o400,
            f"{phase}: probe mode is not exactly 0400")
    require(0 <= metadata.st_size <= MAX_CONTROL_BYTES and metadata.st_size == expected_size,
            f"{phase}: probe size changed or exceeds the control bound")
    data = probe.read_bytes()
    require(len(data) == expected_size and sha(data) == expected_digest,
            f"{phase}: probe bytes or digest changed")
    require(nonce not in data, f"{phase}: probe contains the nonce value")
