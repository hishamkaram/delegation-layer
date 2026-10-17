"""Fixture data and validation over supplied facts."""

from pathlib import Path
import json


SCHEMA_VERSION = 1


SUPERVISOR_VERSION = "4.0.4"


PREDICATE = "fixture:test"


MODE = "read-only"


FIXTURE_VERSION = "2"


FIXTURE_TIME = "2026-09-13T20:00:00.123456789Z"


PREDICATE_SHA256 = "8f42ad8ce7677acab2f694c1e014a918edaf8ae8fac7a4bbbdc51603c6adbe93"


OBSERVATION_BOUND = 5.0


CASE_TIMEOUT = 20


MAX_CONTROL_BYTES = 1 << 20


PARENT_EXIT_BUDGET = "5s"


PARENT_EXIT_CHILD_LIFETIME_MS = 30_000


class HarnessFailure(RuntimeError):
    """A required acceptance observation was absent or incorrect."""


def require_true(condition, message):
    if not condition:
        raise HarnessFailure(message)


def yaml_string(value):
    return json.dumps(str(value), ensure_ascii=False)


def private_pueue_yaml(base):
    """Return ordinary YAML accepted by the strict pueue parser.

    The fake supervisor has a sibling JSON control file.  This document is a
    separate production-style -c input, so its path and digest are part of the
    saved binding.
    """
    base = Path(base)
    values = {
        "pueue_directory": base / "supervisor-state",
        "runtime_directory": base / "supervisor-run",
        "unix_socket_path": base / "supervisor-run" / "pueue.sock",
        "alias_file": base / "aliases.yml",
        "pid_path": base / "supervisor-run" / "pueue.pid",
        "shared_secret_path": base / "supervisor-run" / "secret",
        "daemon_cert": base / "supervisor-run" / "cert.pem",
        "daemon_key": base / "supervisor-run" / "key.pem",
    }
    lines = [
        "shared:",
        "  pueue_directory: " + yaml_string(values["pueue_directory"]),
        "  runtime_directory: " + yaml_string(values["runtime_directory"]),
        "  unix_socket_path: " + yaml_string(values["unix_socket_path"]),
        "  alias_file: " + yaml_string(values["alias_file"]),
        "  use_unix_socket: true",
        "  unix_socket_permissions: 448",
        "  host: " + yaml_string("127.0.0.1"),
        "  port: " + yaml_string("6924"),
        "  pid_path: " + yaml_string(values["pid_path"]),
        "  shared_secret_path: " + yaml_string(values["shared_secret_path"]),
        "  daemon_cert: " + yaml_string(values["daemon_cert"]),
        "  daemon_key: " + yaml_string(values["daemon_key"]),
        "client:",
        "  show_confirmation_questions: false",
        "daemon:",
        "  callback: null",
        "  env_vars: {}",
        "  shell_command:",
        "    - " + yaml_string("/bin/sh"),
        "    - " + yaml_string("-c"),
        "    - " + yaml_string("{{ pueue_command_string }}"),
    ]
    return ("\n".join(lines) + "\n").encode()


def status_job(numeric_id, label, state):
    if state == "stashed":
        status = {"Stashed": {"enqueue_at": None}}
    elif state == "queued":
        status = {"Queued": {"enqueued_at": FIXTURE_TIME}}
    elif state == "running":
        status = {"Running": {"enqueued_at": FIXTURE_TIME, "start": FIXTURE_TIME}}
    elif state == "ended":
        status = {
            "Done": {
                "enqueued_at": FIXTURE_TIME,
                "start": FIXTURE_TIME,
                "end": FIXTURE_TIME,
                "result": "Success",
            }
        }
    elif state == "locked":
        status = {
            "Locked": {
                "previous_status": {
                    "Queued": {"enqueued_at": FIXTURE_TIME},
                },
            }
        }
    else:
        raise HarnessFailure("unsupported fake status state: " + state)
    return {
        "id": numeric_id,
        "created_at": FIXTURE_TIME,
        "original_command": "delegate-run --root /tmp/fixture",
        "command": "delegate-run --root /tmp/fixture",
        "path": "/tmp",
        "envs": {},
        "group": "default",
        "dependencies": [],
        "priority": 0,
        "label": label,
        "status": status,
    }


def status_document(rows):
    return {"tasks": {str(row["id"]): row for row in rows}, "groups": {}}


def canonical_json_bytes(value):
    """Match task.MarshalCanonical for the ASCII manifest values below."""
    return (json.dumps(value, separators=(",", ":"), ensure_ascii=False) + "\n").encode()


def label_parts(label):
    prefix = "delegate:"
    require_true(label.startswith(prefix), "fake add label lacks delegate prefix")
    fields = label[len(prefix):].split(":")
    require_true(len(fields) == 2, "fake add label has unexpected identity")
    root_id, task_id = fields
    require_true(len(root_id) == 32 and len(task_id) == 32,
                 "fake add label has malformed IDs")
    return root_id, task_id


def add_entry_label(entry):
    argv = entry.get("argv")
    require_true(entry.get("verb") == "add" and isinstance(argv, list),
                 "fake add receipt has invalid command identity")
    positions = [index for index, value in enumerate(argv) if value == "--label"]
    require_true(len(positions) == 1 and positions[0] + 1 < len(argv),
                 "fake add receipt has invalid label arguments")
    label = argv[positions[0] + 1]
    require_true(isinstance(label, str) and label,
                 "fake add receipt has an empty label")
    return label
