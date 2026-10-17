"""Private handoff external supervisor control I/O."""

import json
from scripts.delegate_acceptance.private_cli.supervisor import pueue_command
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.directories.policy import require


def registered_process_handle(processes, assigned=None):
    """Resolve the one owned process from the canonical Processes registry."""
    entries = tuple(processes.entries)
    require(len(entries) <= 1, "external supervisor process registry has multiple entries")
    if assigned is not None:
        require(
            any(entry is assigned for entry in entries),
            "external supervisor process was not retained by its canonical registry",
        )
    return entries[0] if entries else None


class ExternalSupervisorControl:
    """Private handoff I/O over the existing canonical owners."""

    def __init__(
        self,
        external_processes,
        client,
        env,
        external_config,
        external_endpoint,
        external_pid_path,
        capture_root,
        evidence,
    ):
        self.external_processes = external_processes
        self.client = client
        self.env = env
        self.external_config = external_config
        self.external_endpoint = external_endpoint
        self.external_pid_path = external_pid_path
        self.capture_root = capture_root
        self.evidence = evidence
        self.external_process = None

    def wait_external_ready(self):
        process = registered_process_handle(self.external_processes, self.external_process)
        require(process is not None, "fixture external supervisor process was not registered")
        if process.poll() is not None:
            raise RuntimeError("fixture external supervisor exited before readiness")
        try:
            snapshot = pueue_snapshot(
                self.client,
                self.external_config,
                self.env,
                phase="external-supervisor-observation",
                capture_root=self.capture_root,
            )
        except (OSError, RuntimeError, UnicodeDecodeError, json.JSONDecodeError):
            return False
        return isinstance(snapshot.get("groups"), dict)

    def remember_external_process(self, process):
        self.external_process = process

    def record_external_process_registry(self):
        entries = tuple(self.external_processes.entries)
        pending = self.external_processes.pending()
        self.evidence["cleanup"]["external_processes"] = {"registered_count": len(entries), "pending": pending}
        return pending

    def shutdown_external(self):
        process = registered_process_handle(self.external_processes, self.external_process)
        if process is None:
            return
        if process.poll() is None:
            pueue_command(
                self.client,
                self.external_config,
                self.env,
                ["shutdown"],
                phase="external-supervisor-shutdown",
                capture_root=self.capture_root,
            )
            process.wait(timeout=30, expected=0)
        require(
            process.result is not None and process.result["natural_wait"],
            "fixture external supervisor did not exit naturally",
        )
        require(
            not self.external_endpoint.exists() and (not self.external_pid_path.exists()),
            "fixture external supervisor endpoint remained after shutdown",
        )
        self.evidence["supervisors"]["external"].update(
            {"daemon_pid": process.result["pid"], "process": process.result, "daemon_shutdown": "verified"}
        )
        self.evidence["cleanup"]["external_daemon"] = "verified"
        self.record_external_process_registry()
