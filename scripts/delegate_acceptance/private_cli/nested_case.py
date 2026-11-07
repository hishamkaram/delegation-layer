"""One case's fixture resources, admissions, observations and closure edges."""

from contextlib import ExitStack
import hashlib
import json
import time
import uuid

from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.evidence.records import _read_control_record
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.private_cli.admission import observe_created_admission
from scripts.delegate_acceptance.private_cli.commands import run_json
from scripts.delegate_acceptance.private_cli.nested_observation import (
    await_record, observe_native, await_actor_closure, bound_private_client, prove_task_absence,
    compare_native_tokens, fixture_launch,
    observe_bootstrap_task_entry,
)
from scripts.delegate_acceptance.lifetime.closure import validate_managed_lifetime_closure
from scripts.delegate_acceptance.private_cli.supervisor import pueue_snapshot
from scripts.delegate_acceptance.private_cli.nested_policy import queue_ended_evidence
from scripts.delegate_acceptance.private_cli.receipt import resource_admission_evidence


class NestedCase:
    """FixtureLifetime owns resources; this case holds their existing handles."""

    def __init__(self, args, lifetime, fixture, receipts, receipt):
        self.args, self.lifetime, self.fixture = args, lifetime, fixture
        self.receipts, self.receipt = receipts, receipt
        self.controls = lifetime.ownership.create("nested controls", lifetime.base / "nested-controls", lifetime.base)
        captures = lifetime.ownership.create("nested command captures", lifetime.base / "nested-captures", lifetime.base)
        self.a = fixture.state
        self.b = self.a if args.nested_topology == "shared-warm" else lifetime.base / "nested-state"
        self.b_ids = lifetime.attempted_task_ids if self.b == self.a else []
        self.b_token = None
        self.creator_fenced = False
        self.configuration = {
            "controls": str(self.controls.directory), "capture": str(captures.directory),
            "capture_identity": list(captures.identity), "delegate": str(lifetime.old_install / "delegate"),
            "native_fixture": str(args.native_fixture.resolve(strict=True)), "workspace": str(fixture.workspace),
            "leader_state": str(self.a), "worker_state": str(self.b),
            "waiters": str(fixture.waiter_directory), "nonce": fixture.lease_nonce,
            "observer_environment": dict(lifetime.env),
            **{role: uuid.uuid4().hex for role in ("leader", "w1", "w2")},
        }
        self.responses = {}
        # Root owns the cleanup phase bound. Every later proof/control reads it;
        # no nested observer renews the finalizer's absolute deadline.
        self.cleanup_deadline = None
        self.leases = ExitStack()
        self.evidence = {"topology": args.nested_topology, "expected_backend": args.expected_backend,
                         "actors": {}, "milestones": {}, "cleanup": {"status": "pending"}}
        receipt.add_details(nested_survival=self.evidence)
        lifetime.prepare_cleanup = self.cleanup
        lifetime.closure_reader = self.read_closure
        if self.b != self.a:
            lifetime.managed_state_roots.append((self.b, self.b_ids))
            lifetime.supervisor_cleanups.append({"delegate": lifetime.old_install / "delegate", "client": lifetime.old_libexec / "pueue",
                "config": self.b / ".supervisor" / "pueue.yml", "env": lifetime.env, "state": self.b,
                "task_ids": self.b_ids})
        self._write_configuration()

    def _write_configuration(self):
        path = self.controls.directory / "configuration.json"
        for role in ("leader", "w1", "w2"):
            brief = self.controls.directory / (role + ".md")
            brief.write_text("DELEGATE_NESTED_SURVIVAL\n" + json.dumps({"role": role, "configuration": str(path)}))
            brief.chmod(0o600)
            self.configuration[role + "_brief"] = str(brief)
        write_json(path, self.configuration)

    def command(self, state, arguments, name, *, expected=(0,), deadline=None):
        return run_json(self.lifetime.old_install / "delegate", self.lifetime.env,
                        ["--root", str(state), *arguments, "--json"], expected=expected,
                        timeout=45 if deadline is None else None, deadline=deadline,
                        receipt=self.receipts / (name + ".json"), capture_response=True,
                        phase="nested-external-observer", capture_root=self.lifetime.capture_token)

    def dispatch(self, state, task_id, brief, budget, name):
        if name == "warm-b":
            require(self.args.nested_topology == "other-warm" and state == self.b and self.b != self.a
                    and self.b_token is None, "warm B bootstrap root was already prepared or changed")
            self.b_token = self.lifetime.ownership.create("warm B state", self.b, self.lifetime.base)
        ids = self.lifetime.attempted_task_ids if state == self.a else self.b_ids
        ids.append(task_id)
        response = self.command(state, ["dispatch", "--provider", "pi:json", "--permission", "read-only",
                                "--cwd", str(self.fixture.workspace), "--brief", str(brief),
                                "--id", task_id, "--budget", budget], name, expected=(0, 1))
        if response.get("task_id") == task_id and response.get("task_record") == "created":
            self.responses[name] = response
        else:
            failure = response.get("failure")
            self.evidence["failed_admission"] = {
                "name": name, "requested_task_id": task_id,
                **{key: response.get(key) for key in
                   ("schema_version", "command", "root_id", "task_id", "task_record", "admission")},
                "failure": {key: failure[key] for key in ("code", "stage", "reason_code", "next_action")
                            if key in failure} if isinstance(failure, dict) else {},
            }
            resource = resource_admission_evidence(response)
            if resource is not None and "diagnostics" in resource:
                self.evidence["failed_admission"]["resource_admission"] = resource
        require(response.get("task_id") == task_id and response.get("task_record") == "created",
                "nested admission was not created; never redispatch")
        self.command(state, ["status", task_id], name + "-status")
        observe_created_admission(self.lifetime.old_install / "delegate", state, self.lifetime.env, response,
                                 self.receipts / (name + ".json"), capture_root=self.lifetime.capture_token)
        if name == "leader":
            self.receipt.increment("dispatch_completed_count")
            self.receipt.increment("admitted_task_count")
        return response

    def nested_response(self, role):
        deadline = time.monotonic() + 60
        await_record(self.controls.directory / (role + "-control-ended.json"), deadline)
        capture = await_record(self.controls.directory / (role + "-dispatch.json"), deadline)
        response = capture.get("response")
        require(capture.get("exit_code") in (0, 1) and isinstance(response, dict)
                and response.get("task_record") == "created" and response.get("admission") == "admitted"
                and response.get("task_id") == self.configuration[role], "nested admission was not proven")
        if response["task_id"] not in self.b_ids:
            self.b_ids.append(response["task_id"])
        self.responses[role] = response
        self.receipt.increment("dispatch_completed_count")
        self.receipt.increment("admitted_task_count")
        return response

    def collect(self, state, response, name):
        result = self.command(state, ["collect", response["task_id"], "--watch", "30s"], name)
        require(result.get("task_id") == response["task_id"] and result.get("root_id") == response["root_id"]
                and result.get("publication") == "committed", "nested result was not committed for the same task")
        outcome, payload = result.get("outcome"), result.get("payload")
        require(isinstance(outcome, dict) and isinstance(payload, dict) and outcome.get("verdict") == "committed"
                and outcome.get("payload") == payload and payload.get("basename") == "result.txt",
                "nested result omitted its accepted payload descriptor")
        path = state / "tasks" / response["task_id"] / "result.txt"
        require(path.is_file() and not path.is_symlink(), "nested result payload is unsafe")
        data = path.read_bytes()
        require(len(data) == payload.get("length") and hashlib.sha256(data).hexdigest() == payload.get("sha256")
                and data.decode().strip() == "private supervisor acceptance passed", "nested accepted result mismatch")
        closure = self.close(state, response, time.monotonic() + 30)
        if response["task_id"] in {self.configuration["w1"], self.configuration["w2"]}:
            self.receipt.increment("collected_task_count")
        return {"task_id": response["task_id"], "publication": "committed", "payload": payload, "closure": closure}

    def close(self, state, response, deadline):
        closure = await_actor_closure(self, state, response, deadline)
        client, environment = bound_private_client(state, self.lifetime.env)
        receipt = _read_control_record(state / "tasks" / response["task_id"] / "supervisor.ref.json", "admission receipt")
        binding = _read_control_record(state / ".supervisor" / "supervisor.identity.json", "published binding")["binding"]
        if not isinstance(binding, dict):
            raise AcceptanceFailure("queue closure observation changed admitted supervisor identity")
        require(receipt.get("root_id") == response["root_id"] and receipt.get("task_id") == response["task_id"]
                and all(receipt.get(key) == binding.get(key) for key in ("config_path", "config_digest", "endpoint")),
                "queue closure observation changed admitted supervisor identity")
        while time.monotonic() < deadline:
            snapshot = pueue_snapshot(client, state / ".supervisor" / "pueue.yml", environment,
                                      phase="nested-external-observer", capture_root=self.lifetime.capture_token)
            ended = queue_ended_evidence(snapshot, receipt, closure)
            if ended is not None:
                closure["queue_ended"] = ended
                return closure
            time.sleep(0.02)
        raise TimeoutError("canonically closed task queue row remained active")

    def native(self, pid, *, scope="", expected_start="", deadline=None):
        if self.cleanup_deadline is not None:
            deadline = self.cleanup_deadline if deadline is None else min(deadline, self.cleanup_deadline)
        return observe_native(self.configuration, self.lifetime.capture_token, pid, scope=scope,
                              expected_start=expected_start, deadline=deadline)

    def tokens_match(self, observed, persisted):
        return compare_native_tokens(self.configuration, self.lifetime.capture_token, observed, persisted,
                                     deadline=self.cleanup_deadline)

    def match_launch(self, state, response, identity):
        return fixture_launch(state, response, identity, tokens_match=self.tokens_match)

    def prove_unlaunched(self, state, root, task_id):
        return prove_task_absence(self.configuration, self.lifetime.capture_token, state, root, task_id,
                                   deadline=self.cleanup_deadline, closed=self.lifetime.closed_roots.get(state))

    def read_closure(self, state, task_ids):
        return validate_managed_lifetime_closure(state, self.cleanup_task_ids(state, task_ids),
                                                 prove_unlaunched=self.prove_unlaunched)

    def cleanup_task_ids(self, state, task_ids):
        """Derive targets afresh; attempted IDs and prior observations are never absence authority."""
        requested = tuple(task_ids)
        if not self.creator_fenced:
            return requested
        failed = self.evidence.get("failed_admission")
        if not isinstance(failed, dict) or failed.get("task_record") == "created":
            return requested
        name = failed.get("name")
        candidate = failed.get("requested_task_id")
        leader = self.configuration["leader"]
        if name in {"warm-a", "warm-b"} and leader in self.lifetime.attempted_task_ids:
            return requested
        if name == "warm-a" and state == self.a:
            attempted, token = self.lifetime.attempted_task_ids, self.lifetime.state_token
        elif name == "warm-b" and state == self.b and self.b != self.a:
            attempted, token = self.b_ids, self.b_token
        elif name == "leader" and state == self.a:
            if candidate != leader or "leader" in self.responses or any(
                    self.configuration[role] in ids for role in ("w1", "w2")
                    for ids in (self.lifetime.attempted_task_ids, self.b_ids)):
                return requested
            attempted, token = self.lifetime.attempted_task_ids, self.lifetime.state_token
        else:
            return requested
        if not attempted or candidate != attempted[-1] or failed.get("task_id") != candidate:
            return requested
        # failed_admission exists only after the direct control's capture-owner
        # Wait. Warm refusals precede any leader attempt. An absent, unreconciled
        # leader cannot have launched the sole creator of nested workers.
        require(token is not None, "bootstrap task observation lacks its original root owner")
        proof = observe_bootstrap_task_entry(self.configuration, self.lifetime.capture_token, state,
                                            failed.get("root_id"), candidate, token,
                                            deadline=self.cleanup_deadline)
        require(proof["entry_exists"] is False, "refused bootstrap task entry is present; retain root")
        self.evidence["cleanup"]["refused_not_created"] = proof
        return tuple(task_id for task_id in requested if task_id != candidate)

    def cleanup(self, deadline, disposal):
        from scripts.delegate_acceptance.private_cli.nested_cleanup import cleanup_case
        failure = None
        self.cleanup_deadline = deadline
        try:
            cleanup_case(self, deadline, disposal)
        except BaseException as error:
            failure = error
            raise
        finally:
            try:
                self.leases.close()
            except BaseException as lease_error:
                if failure is not None:
                    raise RuntimeError(str(failure) + "; fixture lease cleanup failed: " + str(lease_error)) from failure
                raise

    def release(self, role):
        path = self.controls.directory / (role + "-release")
        if not path.exists():
            path.touch(mode=0o600, exist_ok=False)
