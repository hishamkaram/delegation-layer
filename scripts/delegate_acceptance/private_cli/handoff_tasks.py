"""Private handoff task command I/O."""

from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_TASK_BUDGET
from scripts.delegate_acceptance.private_cli.leases import LiveFIFOLeaseCohort
from scripts.delegate_acceptance.private_cli.capacity_records import capacity_lease_specifications
import hashlib
import json
from scripts.delegate_acceptance.private_cli.admission import observe_created_admission
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.private_cli.commands import run_json
import time
from scripts.delegate_acceptance.private_cli.supervisor import wait_for_pueue_state
from scripts.delegate_acceptance.private_cli.wrappers import wrapper_call_records


class HandoffTaskCommands:
    """Private handoff I/O over the existing canonical owners."""

    def __init__(
        self,
        *, delegate,
        env,
        state,
        workspace,
        waiting_brief,
        receipts,
        capture_root,
        run_receipt,
        waiter_directory,
        lease_nonce,
        external_config,
    ):
        self.delegate = delegate
        self.env = env
        self.state = state
        self.workspace = workspace
        self.waiting_brief = waiting_brief
        self.receipts = receipts
        self.capture_root = capture_root
        self.run_receipt = run_receipt
        self.waiter_directory = waiter_directory
        self.lease_nonce = lease_nonce
        self.external_config = external_config

    def write_snapshot(self, name, snapshot):
        path = self.receipts / name
        path.write_text(json.dumps(snapshot, sort_keys=True, indent=2) + "\n")
        return path.name

    def external_client_calls(self):
        calls = wrapper_call_records(self.capture_root.directory / "old-client-starts-calls", "client", "old")
        return [record for record in calls if str(self.external_config) in json.dumps(record, sort_keys=True)]

    def dispatch_fixture(self, task_id, command_prefix, receipt_name):
        response = run_json(
            self.delegate,
            self.env,
            [
                *command_prefix,
                "dispatch",
                "--provider",
                "pi:json",
                "--brief",
                str(self.waiting_brief),
                "--cwd",
                str(self.workspace),
                "--id",
                task_id,
                "--permission",
                "read-only",
                "--budget",
                EXTERNAL_INSPECTION_HANDOFF_TASK_BUDGET,
                "--json",
            ],
            expected=(0, 1),
            receipt=self.receipts / receipt_name,
            phase="external-handoff-dispatch",
            capture_root=self.capture_root,
        )
        if response.get("task_id") != task_id or response.get("task_record") != "created":
            raise RuntimeError("external handoff dispatch did not create the requested task")
        if response.get("admission") != "admitted":
            response = observe_created_admission(
                self.delegate,
                self.state,
                self.env,
                response,
                self.receipts / receipt_name,
                phase="external-handoff-admission-observation",
                command_prefix=command_prefix,
                capture_root=self.capture_root,
            )
        if response.get("task_id") != task_id or response.get("admission") != "admitted":
            raise RuntimeError("external handoff admission did not confirm the same task")
        self.run_receipt.increment("dispatch_completed_count")
        self.run_receipt.increment("admitted_task_count")
        return response

    def live_cohort(self, response, supervisor_client, supervisor_config, phase):
        label = "delegate:" + response["root_id"] + ":" + response["task_id"]
        wait_for_pueue_state(
            supervisor_client,
            supervisor_config,
            self.env,
            label,
            "Running",
            phase=phase,
            capture_root=self.capture_root,
        )
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            specifications = capacity_lease_specifications(
                self.state, [response], self.waiter_directory, self.lease_nonce
            )
            if len(specifications) == 1 and specifications[0]["path"].exists():
                cohort = LiveFIFOLeaseCohort.acquire(specifications, self.lease_nonce)
                cohort.assert_live()
                return (specifications, cohort)
            time.sleep(0.05)
        raise RuntimeError("fixture task did not reach a live lease: " + response["task_id"])

    def task_meta(self, task_id):
        path = self.state / "tasks" / task_id / "meta.json"
        require(path.is_file() and (not path.is_symlink()), "task metadata is missing: " + task_id)
        data = path.read_bytes()
        return (path, data, hashlib.sha256(data).hexdigest())

    @property
    def external_prefix(self):
        return ["--root", str(self.state), "--pueue-config", str(self.external_config)]

    @property
    def private_prefix(self):
        return ["--root", str(self.state)]
