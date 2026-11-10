"""Finite provider behavior; subprocess capture stays with the canonical owner."""

import json
import os
from pathlib import Path
import signal
import time

from scripts.acceptance.evidence.files import write_json
from scripts.acceptance.evidence.records import _read_control_record
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.capture import borrow_capture_root
from scripts.acceptance.private_cli.capture import run_captured_text_command
from scripts.acceptance.private_cli.commands import run_json
from scripts.acceptance.private_cli.nested.policy import timestamp_ns
from scripts.acceptance.private_cli.nested.policy import positive_admission_guard_arguments


def fixture_environment(configuration, **values):
    # Provider environment belongs to its native scope. Preserve its marker for
    # the ordinary descendant; Delegate's own CLI owns daemon environment policy.
    environment = dict(os.environ)
    environment.update({"DELEGATE_ACCEPTANCE_PHASE": "nested-fixture-control", **values})
    return environment


def native_observation(configuration, capture, pid, output, *, mode="inspect", deadline=None, **extra):
    environment = fixture_environment(configuration, DELEGATE_NESTED_FIXTURE_MODE=mode,
                                      DELEGATE_NESTED_FIXTURE_PID=str(pid),
                                      DELEGATE_NESTED_FIXTURE_OUTPUT=str(output), **extra)
    bound = min(deadline, time.monotonic() + 10) if deadline is not None else None
    result = run_captured_text_command([configuration["native_fixture"],
                                       "-test.run=^TestNestedSurvivalFixture$"], environment,
                                      10 if bound is None else None, deadline=bound, capture_root=capture)
    require(result.returncode == 0, "native nested fixture observation failed: " + result.stderr[-1000:])


def own_identity(configuration, capture, output):
    native_observation(configuration, capture, os.getpid(), output)
    identity = _read_control_record(output, "fixture self identity")
    require(identity.get("pid") == os.getpid() and identity.get("present") is True,
            "native self-observation changed the fixture PID")
    return identity


def emit_result():
    message = {"role": "assistant", "content": [{"type": "text", "text": "private supervisor acceptance passed"}],
               "stopReason": "stop"}
    events = [{"type": "session", "id": "00000000-0000-4000-8000-000000000001"},
              {"type": "agent_start"}, {"type": "turn_start"},
              {"type": "message_start", "message": {"role": "assistant", "content": []}},
              {"type": "message_end", "message": message},
              {"type": "turn_end", "message": message, "toolResults": []},
              {"type": "agent_end", "messages": [message]}]
    for event in events:
        print(json.dumps(event), flush=True)


def wait_gate(path, fence, deadline_ns, configuration, capture):
    while time.time_ns() < deadline_ns:
        if fence.exists():
            # Acknowledge only between completed control invocations. Keep the
            # leader alive while the outside cleanup owner closes its children.
            acknowledgement = fence.with_name("fenced.json")
            if not acknowledgement.exists():
                observation = fence.with_name("fenced-identity.json")
                identity = own_identity(configuration, capture, observation)
                write_json(acknowledgement, {"observed_at_unix_ns": time.time_ns(), "pid": os.getpid(),
                           "process_start": identity["process_start"], "nonce": configuration["nonce"]})
            while time.time_ns() < deadline_ns:
                if fence.with_name("leader-release").exists():
                    return False
                time.sleep(0.02)
            raise TimeoutError("fenced leader reached its own fixture bound")
        if path.exists():
            return True
        time.sleep(0.02)
    raise TimeoutError("nested fixture gate expired")


def dispatch_worker(configuration, capture, role):
    guard_arguments = positive_admission_guard_arguments(configuration.get("cpu_guard"))
    controls = Path(configuration["controls"])
    if (controls / "fence").exists():
        wait_gate(controls / "leader-release", controls / "fence", time.time_ns() + 180_000_000_000,
                  configuration, capture)
        return False
    write_json(controls / (role + "-intent.json"), {"task_id": configuration[role], "observed_at_unix_ns": time.time_ns()})
    try:
        run_json(Path(configuration["delegate"]), fixture_environment(configuration),
                 ["--root", configuration["worker_state"], "dispatch", "--provider", "pi:json",
                  "--permission", "read-only", "--cwd", configuration["workspace"],
                  "--brief", configuration[role + "_brief"], "--id", configuration[role],
                  "--budget", "10m", "--json", *guard_arguments], expected=(0, 1), timeout=45,
                 receipt=controls / (role + "-dispatch.json"), capture_root=capture,
                 phase="nested-fixture-control", capture_response=True)
    finally:
        write_json(controls / (role + "-control-ended.json"), {"observed_at_unix_ns": time.time_ns()})
    return True


def run_leader(configuration, capture):
    controls = Path(configuration["controls"])
    intent = _read_control_record(Path(configuration["leader_state"]) / "tasks" / configuration["leader"]
                                / "lifetime" / "intent.json", "leader intent")
    deadline = timestamp_ns(intent["deadline"])
    own_identity(configuration, capture, controls / "leader-identity.json")
    native_observation(configuration, capture, os.getpid(), controls / "descendant.json", mode="orphan-parent",
                       DELEGATE_NESTED_FIXTURE_RELEASE=str(controls / "descendant-release"),
                       DELEGATE_NESTED_FIXTURE_DEADLINE_NS=str(deadline + 60_000_000_000))
    if not dispatch_worker(configuration, capture, "w1"):
        return
    if not wait_gate(controls / "paused", controls / "fence", deadline, configuration, capture):
        return
    if not dispatch_worker(configuration, capture, "w2"):
        return
    wait_gate(controls / "leader-release", controls / "fence", deadline + 30_000_000_000, configuration, capture)


def run_worker(configuration, capture, role):
    controls = Path(configuration["controls"])
    observation = controls / (role + "-identity.json")
    identity = own_identity(configuration, capture, observation)
    fifo = Path(configuration["waiters"]) / (configuration["nonce"] + "-" + str(os.getpid()) + ".fifo")
    os.mkfifo(fifo, 0o600)
    descriptor = -1
    try:
        descriptor = os.open(fifo, os.O_RDWR | os.O_NONBLOCK | os.O_NOFOLLOW | os.O_CLOEXEC)
        os.write(descriptor, (configuration["nonce"] + "\t" + str(os.getpid()) + "\n").encode())
        deadline = time.monotonic() + 600
        answered = False
        while time.monotonic() < deadline:
            if (controls / (role + "-release")).exists():
                emit_result()
                return
            if role == "w1" and not answered and (controls / "challenge.json").exists():
                challenge = _read_control_record(controls / "challenge.json", "nonce challenge")
                write_json(controls / "reply.json", {"nonce": challenge["nonce"], "pid": os.getpid(),
                           "process_start": identity["process_start"], "observed_at_unix_ns": time.time_ns()})
                answered = True
            time.sleep(0.02)
        raise TimeoutError("nested worker fixture backstop expired")
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        fifo.unlink(missing_ok=True)


def main(prompt):
    sentinel, payload = prompt.split("\n", 1)
    require(sentinel == "DELEGATE_NESTED_SURVIVAL", "invalid nested fixture prompt")
    request = json.loads(payload)
    require(isinstance(request, dict) and request.get("role") in {"leader", "w1", "w2"}
            and isinstance(request.get("configuration"), str), "invalid nested fixture request")
    configuration = _read_control_record(Path(request["configuration"]), "nested fixture configuration")

    def stopped(signum, _frame):
        raise SystemExit(128 + signum)

    for handled in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
        signal.signal(handled, stopped)
    with borrow_capture_root(configuration["capture"], configuration["capture_identity"]) as capture:
        if request["role"] == "leader":
            run_leader(configuration, capture)
        else:
            run_worker(configuration, capture, request["role"])
