"""One case's fixture resources, admissions, observations and closure edges."""

from contextlib import ExitStack
import hashlib
import json
import time
import uuid

from scripts.acceptance.evidence.files import write_json
from scripts.acceptance.evidence.records import _read_control_record
from scripts.acceptance.evidence.cpu_snapshot import CPU_SNAPSHOT_ARGV
from scripts.acceptance.evidence.cpu_snapshot import CPU_SNAPSHOT_BYTES
from scripts.acceptance.evidence.cpu_snapshot import CPU_SNAPSHOT_TIMEOUT_SECONDS
from scripts.acceptance.evidence.cpu_snapshot import confirmed_host_cpu_response
from scripts.acceptance.evidence.cpu_snapshot import cpu_snapshot_rows
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.admission import observe_created_admission
from scripts.acceptance.private_cli.commands import run_json
from scripts.acceptance.private_cli.nested.observation import await_record
from scripts.acceptance.private_cli.nested.observation import observe_native
from scripts.acceptance.private_cli.nested.observation import await_actor_closure
from scripts.acceptance.private_cli.nested.observation import bound_private_client
from scripts.acceptance.private_cli.nested.observation import prove_task_absence
from scripts.acceptance.private_cli.nested.observation import compare_native_tokens
from scripts.acceptance.private_cli.nested.observation import fixture_launch
from scripts.acceptance.private_cli.nested.observation import observe_bootstrap_task_entry
from scripts.acceptance.lifetime.closure import validate_managed_lifetime_closure
from scripts.acceptance.private_cli.supervisor import pueue_snapshot
from scripts.acceptance.private_cli.nested.policy import queue_ended_evidence
from scripts.acceptance.private_cli.nested.policy import positive_admission_guard_arguments
from scripts.acceptance.private_cli.receipt import resource_admission_evidence
from scripts.acceptance.processes.operations import run_bounded_command
from scripts.acceptance.processes.contracts import command_closure_unproven


class NestedCase:
    """FixtureLifetime owns resources; this case holds their existing handles."""

    def __init__(self, args, lifetime, fixture, receipts, receipt):
        self.args, self.lifetime, self.fixture = args, lifetime, fixture
        self.receipts, self.receipt = receipts, receipt
        guard_arguments = positive_admission_guard_arguments(args.nested_cpu_guard)
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
            "cpu_guard": args.nested_cpu_guard,
            **{role: uuid.uuid4().hex for role in ("leader", "w1", "w2")},
        }
        self.responses = {}
        # Root owns the cleanup phase bound. Every later proof/control reads it;
        # no nested observer renews the finalizer's absolute deadline.
        self.cleanup_deadline = None
        self.leases = ExitStack()
        self.evidence = {"topology": args.nested_topology, "expected_backend": args.expected_backend,
                         "actors": {}, "milestones": {}, "cleanup": {"status": "pending"},
                         "admission_guard_scope": "positive-fixture-admissions",
                         "admission_guards": {"cpu": "disabled-by-caller" if guard_arguments else "enabled",
                                              "memory": "enabled"}}
        if guard_arguments:
            self.evidence["admission_guard_limit"] = (
                "CPU admission disabled by caller; host load source and fixture-descendant closure are unproven.")
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

    def _record_failed_admission(self, name, task_id, response, *, capture=None):
        observed_at = time.monotonic()
        public = response if isinstance(response, dict) else {}
        projected = {"name": name, "requested_task_id": task_id, "failure": {}}
        for key in ("schema_version", "command", "root_id", "task_id", "task_record", "admission"):
            value = public.get(key)
            if (value is None or
                    (key == "schema_version" and type(value) is int and value.bit_length() <= 64) or
                    (key != "schema_version" and isinstance(value, str) and len(value) <= 256)):
                projected[key] = value
        failure = public.get("failure")
        if isinstance(failure, dict):
            projected["failure"] = {key: failure[key] for key in ("code", "stage", "reason_code", "next_action")
                                    if isinstance(failure.get(key), str) and len(failure[key]) <= 256}
        resource = resource_admission_evidence(public)
        if resource is not None and "diagnostics" in resource:
            projected["resource_admission"] = resource
        if capture is not None:
            code = capture.get("exit_code")
            projected["exit_code"] = code if type(code) is int and -(2**31) <= code < 2**31 else None
            projected["response_present"] = isinstance(response, dict)
        self.evidence["failed_admission"] = projected
        snapshot = self._capture_refusal_cpu(name, task_id, public, capture, observed_at)
        if snapshot is not None:
            projected["host_cpu_snapshot"] = snapshot

    def _capture_refusal_cpu(self, name, task_id, response, capture, observed_at):
        if not confirmed_host_cpu_response(response, task_id, None):
            return None
        snapshot: dict[str, object] = {
            "method": "native_ps", "timing": "after_resource_refusal",
            "basis": "native_platform_historical_percent", "origin": "driver_failure_observed",
            "admission_intervals_matched": False, "causal_attribution": False,
        }
        reason = "control_completion_unavailable"
        try:
            if capture is None:
                capture = _read_control_record(self.receipts / (name + ".json"), "completed nested control")
            # run_json records the parsed response only after its capture owner
            # has joined the child. An invocation fault cannot authorize a probe.
            require(type(capture.get("exit_code")) is int and capture["exit_code"] == 1
                    and capture.get("failure_class") == "nonzero_exit"
                    and capture.get("natural_wait", True) is True and "invocation_error" not in capture
                    and confirmed_host_cpu_response(capture.get("response"), task_id, response.get("root_id"))
                    and capture.get("response") == response,
                    "nested CPU diagnostic requires a completed matching refusal")
            snapshot["capture_started_offset_ms"] = int((time.monotonic() - observed_at) * 1000)
            reason = "capture_unavailable"
            result = run_bounded_command(
                list(CPU_SNAPSHOT_ARGV), {"LC_ALL": "C"}, cwd=self.fixture.workspace, capture=True,
                timeout=CPU_SNAPSHOT_TIMEOUT_SECONDS, max_output=CPU_SNAPSHOT_BYTES,
            )
            reason = "native_completion_unavailable"
            require(type(result.exit_code) is int and result.exit_code == 0,
                    "native CPU listing did not complete successfully")
            reason = "invalid_output"
            text = result.output.decode("utf-8")
            snapshot["capture_completed_offset_ms"] = int((time.monotonic() - observed_at) * 1000)
            snapshot.update(cpu_snapshot_rows(text), status="observed")
        except BaseException as error:
            # Diagnostics are secondary, including interruption; the original
            # refusal still reaches the existing sequential cleanup and exit.
            snapshot.update(status="unavailable", reason=reason)
            if command_closure_unproven(error):
                snapshot["closure_unproven"] = True
        return snapshot

    def _dispatch_guard_arguments(self, name):
        """Build flags for a real positive admission."""
        return positive_admission_guard_arguments(self.configuration["cpu_guard"])

    def dispatch(self, state, task_id, brief, budget, name):
        if name == "warm-b":
            require(self.args.nested_topology == "other-warm" and state == self.b and self.b != self.a
                    and self.b_token is None, "warm B bootstrap root was already prepared or changed")
            self.b_token = self.lifetime.ownership.create("warm B state", self.b, self.lifetime.base)
        ids = self.lifetime.attempted_task_ids if state == self.a else self.b_ids
        ids.append(task_id)
        response = self.command(state, ["dispatch", "--provider", "pi:json", "--permission", "read-only",
                                "--cwd", str(self.fixture.workspace), "--brief", str(brief),
                                "--id", task_id, "--budget", budget, *self._dispatch_guard_arguments(name)],
                                name, expected=(0, 1))
        if response.get("task_id") == task_id and response.get("task_record") == "created":
            self.responses[name] = response
        else:
            self._record_failed_admission(name, task_id, response)
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
        admitted = (capture.get("exit_code") in (0, 1) and isinstance(response, dict)
                    and response.get("task_record") == "created" and response.get("admission") == "admitted"
                    and response.get("task_id") == self.configuration[role])
        if not admitted:
            self._record_failed_admission(role, self.configuration[role], response, capture=capture)
        require(admitted, "nested admission was not proven")
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
        # Bootstrap failed_admission evidence follows the direct control's capture-owner
        # Wait. Warm refusals precede any leader attempt. An absent, unreconciled
        # leader cannot have launched the sole creator of nested workers.
        require(token is not None, "bootstrap task observation lacks its original root owner")
        proof = observe_bootstrap_task_entry(self.configuration, self.lifetime.capture_token, state,
                                            failed.get("root_id"), candidate, token,
                                            deadline=self.cleanup_deadline)
        require(proof["entry_exists"] is False, "refused bootstrap task entry is present; retain root")
        cleanup = self.evidence["cleanup"]
        if not isinstance(cleanup, dict):
            raise AcceptanceFailure("nested cleanup evidence is not an object")
        cleanup["refused_not_created"] = proof
        return tuple(task_id for task_id in requested if task_id != candidate)

    def cleanup(self, deadline, disposal):
        from scripts.acceptance.private_cli.nested.cleanup import cleanup_case
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
