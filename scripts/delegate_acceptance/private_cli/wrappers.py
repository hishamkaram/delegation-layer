"""Private CLI wrappers boundary."""

from pathlib import Path
import hashlib
import shlex


def install_wrapper(destination, executable, marker, identity, role):
    arguments = marker.with_name(marker.name + "-arguments")
    calls = marker.with_name(marker.name + "-calls")
    if not destination.parent.is_dir() or destination.parent.is_symlink():
        raise RuntimeError("package wrapper parent is not an owned directory: " + str(destination.parent))
    destination.write_text(
        "#!/bin/sh\n"
        "set -eu\n"
        "printf '%s\\n' started >> " + shlex.quote(str(marker)) + "\n"
        "printf '%s\\n' \"$*\" >> " + shlex.quote(str(arguments)) + "\n"
        "printf '%s\\t%s\\t%s\\n' \"${DELEGATE_ACCEPTANCE_PHASE:-unattributed}\" "
        + shlex.quote(role)
        + ' "$*" >> '
        + shlex.quote(str(calls))
        + "\n"
        "exec " + shlex.quote(str(executable)) + ' "$@"\n'
        "# " + identity + "\n"
    )
    destination.chmod(0o700)


def wrapper_call_records(path, role, installation):
    path = Path(path)
    if not path.is_file() or path.is_symlink():
        return []
    records = []
    for line in path.read_text().splitlines():
        fields = line.split("\t", 2)
        if len(fields) != 3:
            raise RuntimeError("private supervisor wrapper call receipt is malformed: " + str(path))
        phase, logged_role, arguments = fields
        if logged_role != role or not phase:
            raise RuntimeError("private supervisor wrapper call identity is invalid: " + str(path))
        tokens = arguments.split()
        command = (
            "startup"
            if role == "daemon"
            else (
                tokens[2] if len(tokens) > 2 and tokens[0] in ("-c", "--config") else tokens[0] if tokens else "empty"
            )
        )
        raw = arguments.encode()
        records.append(
            {
                "installation": installation,
                "phase": phase,
                "role": role,
                "command": command,
                "arguments_bytes": len(raw),
                "arguments_sha256": hashlib.sha256(raw).hexdigest(),
            }
        )
    return records


def wrapper_command_counts(path):
    counts = {}
    for record in wrapper_call_records(path, "client", "unknown"):
        command = record["command"]
        counts[command] = counts.get(command, 0) + 1
    return dict(sorted(counts.items()))
