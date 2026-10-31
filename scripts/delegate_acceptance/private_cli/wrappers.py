"""Private CLI supervisor wrappers: the one owner of their rendered script and call logs."""

from pathlib import Path
import hashlib
import json
import os
import shlex
import stat
import sys

from scripts.delegate_acceptance.private_cli.wrapper_policy import leading_pueue_config


CALL_RECORD_VERSION = 1
CALL_RECORD_KEYS = ("v", "phase", "role", "argv")
# One isolated logger child per wrapper call. It prepares the legacy flat
# arguments line and the canonical JSON call record, then appends the flat
# line first and the record last, each with one open, one checked write and
# one checked close. Any fault exits 125 before the shell reaches its exec;
# a partial append is never retried.
CALL_LOGGER_CODE = """import json,os,sys
def append(path,data):
 f=os.open(path,os.O_WRONLY|os.O_APPEND|os.O_CREAT,0o666)
 try:
  n=os.write(f,data)
 finally:
  os.close(f)
 if n!=len(data):raise OSError("short append")
try:
 p,r,g,c,*a=sys.argv[1:]
 flat=b" ".join(os.fsencode(x) for x in a)+b"\\n"
 d=json.dumps({"v":1,"phase":p,"role":r,"argv":a},ensure_ascii=True,separators=(",",":")).encode("ascii")+b"\\n"
 append(g,flat)
 append(c,d)
except Exception as e:
 os.write(2,b"pueue wrapper logger refused: "+type(e).__name__.encode()+b"\\n");sys.exit(125)
"""


def install_wrapper(destination, executable, marker, identity, role):
    arguments = marker.with_name(marker.name + "-arguments")
    calls = marker.with_name(marker.name + "-calls")
    if not destination.parent.is_dir() or destination.parent.is_symlink():
        raise RuntimeError("package wrapper parent is not an owned directory: " + str(destination.parent))
    logger = shlex.join([sys.executable, "-I", "-S", "-c", CALL_LOGGER_CODE])
    destination.write_text(
        "#!/bin/sh\n"
        "set -eu\n"
        "printf '%s\\n' started >> " + shlex.quote(str(marker)) + "\n"
        + logger
        + ' "${DELEGATE_ACCEPTANCE_PHASE:-unattributed}" '
        + shlex.join([role, str(arguments), str(calls)])
        + ' "$@"\n'
        "exec " + shlex.quote(str(executable)) + ' "$@"\n'
        "# " + identity + "\n"
    )
    destination.chmod(0o700)


def wrapper_call_records(path, role, installation, *, config=None):
    """Return compact records of one wrapper call log, optionally only calls whose leading config is ``config``.

    A log that does not exist yet holds no calls. Any other unreadable, torn or
    non-canonical log raises. Records never carry the raw arguments or config.
    """
    path = Path(path)
    data = _read_call_log(path)
    if data is None:
        return []
    selected = None if config is None else os.fspath(config)
    records = []
    for index, (phase, argv, encoded) in enumerate(_decode_call_log(data, role, path)):
        leading, remaining = (None, tuple(argv))
        if role == "client" or selected is not None:
            try:
                leading, remaining = leading_pueue_config(argv)
            except ValueError as error:
                raise RuntimeError("%s record %d: %s" % (path, index, error)) from error
        if selected is not None and leading != selected:
            continue
        flat = b" ".join(encoded)
        records.append(
            {
                "installation": installation,
                "phase": phase,
                "role": role,
                "command": "startup" if role == "daemon" else remaining[0] if remaining else "empty",
                "arguments_bytes": len(flat),
                "arguments_sha256": hashlib.sha256(flat).hexdigest(),
            }
        )
    return records


def wrapper_command_counts(path):
    counts = {}
    for record in wrapper_call_records(path, "client", "unknown"):
        command = record["command"]
        counts[command] = counts.get(command, 0) + 1
    return dict(sorted(counts.items()))


def _encode_call_record(phase, role, argv):
    """Return the canonical call record line the logger writes, without its newline."""
    record = {"v": CALL_RECORD_VERSION, "phase": phase, "role": role, "argv": list(argv)}
    return json.dumps(record, ensure_ascii=True, separators=(",", ":")).encode("ascii")


def _read_call_log(path):
    """Return the exact bytes of one regular call log, or None when it does not exist."""
    try:
        before = os.lstat(path)
    except FileNotFoundError:
        return None
    if not stat.S_ISREG(before.st_mode):
        raise RuntimeError("private supervisor wrapper call log is not a regular file: " + str(path))
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0))
    except OSError as error:
        raise RuntimeError("private supervisor wrapper call log is unreadable: " + str(path)) from error
    with os.fdopen(descriptor, "rb") as stream:
        opened = os.fstat(stream.fileno())
        if (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            raise RuntimeError("private supervisor wrapper call log changed while opening: " + str(path))
        return stream.read()


def _decode_call_log(data, role, path):
    if data and not data.endswith(b"\n"):
        raise RuntimeError("private supervisor wrapper call log has a torn final record: " + str(path))
    return [
        _decode_call_record(line, role, "%s record %d" % (path, index))
        for index, line in enumerate(data.split(b"\n")[:-1])
    ]


def _unique_object(pairs):
    keys = [key for key, _value in pairs]
    if len(set(keys)) != len(keys):
        raise ValueError("duplicate key")
    return dict(pairs)


def _refuse_constant(name):
    raise ValueError("non-finite number " + name)


def _decode_call_record(line, role, where):
    """Return (phase, argv, encoded argv) of one canonical record whose role is ``role``."""
    try:
        value = json.loads(line.decode("ascii"), object_pairs_hook=_unique_object, parse_constant=_refuse_constant)
    except (UnicodeDecodeError, ValueError) as error:
        raise RuntimeError(where + " is not one ASCII JSON call record") from error
    if not isinstance(value, dict) or tuple(value) != CALL_RECORD_KEYS:
        raise RuntimeError(where + " has unexpected keys")
    version, phase, logged_role, argv = (value[key] for key in CALL_RECORD_KEYS)
    # bool and 1.0 compare equal to 1; only the exact integer is this version.
    if type(version) is not int or version != CALL_RECORD_VERSION:
        raise RuntimeError(where + " has an unsupported version")
    if not isinstance(phase, str) or not phase or logged_role != role:
        raise RuntimeError(where + " has an invalid phase or role identity")
    if not isinstance(argv, list) or not all(isinstance(argument, str) for argument in argv):
        raise RuntimeError(where + " argv is not a list of text")
    try:
        encoded = [os.fsencode(argument) for argument in argv]
    except UnicodeEncodeError as error:
        raise RuntimeError(where + " argv has no exact byte form") from error
    if [os.fsdecode(argument) for argument in encoded] != argv or _encode_call_record(phase, role, argv) != line:
        raise RuntimeError(where + " is not canonical")
    return phase, argv, encoded
