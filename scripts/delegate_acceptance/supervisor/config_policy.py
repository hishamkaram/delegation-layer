"""Pure Pueue config projection and exact Go-compatible config digest."""

from __future__ import annotations

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.encoding import canonical_go_json
from scripts.delegate_acceptance.evidence.validation import require
from pathlib import Path


def config_for(base):
    base = Path(base)
    return {
        "shared": {
            "pueue_directory": str(base / "state"),
            "runtime_directory": str(base / "run"),
            "unix_socket_path": str(base / "run" / "p.sock"),
            "alias_file": str(base / "aliases.yml"),
            "pid_path": str(base / "run" / "p.pid"),
            "shared_secret_path": str(base / "run" / "secret"),
            "daemon_cert": str(base / "run" / "cert.pem"),
            "daemon_key": str(base / "run" / "key.pem"),
            "use_unix_socket": True, "unix_socket_permissions": 384,
            "host": "127.0.0.1", "port": "6924",
        },
        "client": {
            "restart_in_place": False, "read_local_logs": True,
            "show_confirmation_questions": False, "edit_mode": "toml",
            "show_expanded_aliases": False, "dark_mode": False,
            "max_status_lines": None, "status_time_format": "%H:%M:%S",
            "status_datetime_format": "%Y-%m-%d\n%H:%M:%S",
        },
        "daemon": {
            "pause_group_on_failure": False, "pause_all_on_failure": False,
            "compress_state_file": False, "callback": None, "env_vars": {},
            "callback_log_lines": 10,
            "shell_command": ["/bin/sh", "-c", "{{ pueue_command_string }}"],
        },
        "profiles": {},
    }


def resolved_supervisor_config(base: Path) -> dict[str, object]:
    """Map the trusted pueue config source into ResolvedConfig field order."""
    source = config_for(Path(base))
    client = source["client"]
    daemon = source["daemon"]
    shared = source["shared"]
    require(isinstance(client, dict) and isinstance(daemon, dict) and
            isinstance(shared, dict), "supervisor config source is malformed")
    env_vars = daemon["env_vars"]
    require(isinstance(env_vars, dict), "supervisor environment source is malformed")
    return {
        "Client": {
            "RestartInPlace": client["restart_in_place"],
            "ReadLocalLogs": client["read_local_logs"],
            "ShowConfirmationQuestions": client["show_confirmation_questions"],
            "EditMode": client["edit_mode"],
            "ShowExpandedAliases": client["show_expanded_aliases"],
            "DarkMode": client["dark_mode"],
            "MaxStatusLines": client["max_status_lines"],
            "StatusTimeFormat": client["status_time_format"],
            "StatusDatetimeFormat": client["status_datetime_format"],
        },
        "Daemon": {
            "PauseGroupOnFailure": daemon["pause_group_on_failure"],
            "PauseAllOnFailure": daemon["pause_all_on_failure"],
            "CompressStateFile": daemon["compress_state_file"],
            "Callback": daemon["callback"],
            "EnvVars": {key: env_vars[key] for key in sorted(env_vars)},
            "CallbackLogLines": daemon["callback_log_lines"],
            "ShellCommand": daemon["shell_command"],
        },
        "PueueDirectory": shared["pueue_directory"],
        "RuntimeDirectory": shared["runtime_directory"],
        "AliasFile": shared["alias_file"],
        "SocketPath": shared["unix_socket_path"],
        "PIDPath": shared["pid_path"],
        "SecretPath": shared["shared_secret_path"],
        "CertificatePath": shared["daemon_cert"],
        "KeyPath": shared["daemon_key"],
        "SocketPermissions": shared["unix_socket_permissions"],
        "Host": shared["host"],
        "Port": shared["port"],
    }


def resolved_supervisor_config_digest(base: Path) -> str:
    """Hash ResolvedConfig exactly as pueue does, without a record newline."""
    return sha(canonical_go_json(resolved_supervisor_config(base), newline=False))
