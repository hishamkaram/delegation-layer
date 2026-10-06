#!/usr/bin/env python3
"""Record routing calls and optionally return bounded synthetic terminal evidence.

Ordinary routing still invokes the real Delegate binary with provider executables
hidden.  The recovery scenarios are an explicit, disposable fixture mode: the
real CLI parses and validates the request first, then this script supplies a
small terminal response without launching a provider or reading raw output.
"""
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


SYNTHETIC_EVIDENCE = "synthetic-routing-evidence"
STATE_NAME = "routing-state.json"
EVENTS_NAME = "routing-events.jsonl"
TASK_ID_RE = re.compile(r"^[0-9a-f]{32}$")
COMMANDS = ("dispatch", "status", "collect", "recheck", "models")
PROVIDERS = {
    "antigravity:print",
    "ccr:print",
    "claude:print",
    "codex:exec",
    "opencode:run",
    "pi:json",
}
SCENARIOS = {
    "recoverable-rejection": "recovered",
    "still-rejected-recheck": "rejected",
}


def digest_bytes(data):
    return hashlib.sha256(data).hexdigest()


def digest(path):
    return digest_bytes(Path(path).read_bytes())


def read_json(path):
    return json.loads(Path(path).read_text(encoding="utf-8"))


def write_json(path, value):
    path = Path(path)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2, sort_keys=True)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)


def append_json(path, value):
    path = Path(path)
    if path.exists() and path.is_symlink():
        raise RuntimeError("routing evidence log is a symlink")
    with path.open("a", encoding="utf-8") as stream:
        json.dump(value, stream, sort_keys=True)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())


@contextmanager
def state_lock(path):
    """Serialize the tiny fixture state without sharing locks with Delegate."""
    import fcntl

    lock_path = Path(path).with_name(Path(path).name + ".lock")
    with lock_path.open("a+", encoding="utf-8") as stream:
        fcntl.flock(stream.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(stream.fileno(), fcntl.LOCK_UN)


def option_value(argv, name):
    prefix = name + "="
    for index, value in enumerate(argv):
        if value == name:
            if index + 1 >= len(argv):
                return None
            return argv[index + 1]
        if value.startswith(prefix):
            return value[len(prefix):]
    return None


def command_name(argv):
    for value in argv:
        if value in COMMANDS:
            return value
    return None


def requested_root(argv):
    value = option_value(argv, "--root")
    if not value:
        return None
    path = Path(value)
    return path.resolve() if path.is_absolute() else None


def requested_task(argv, command):
    try:
        index = argv.index(command)
    except ValueError:
        return None
    for value in argv[index + 1:]:
        if not value.startswith("-"):
            return value
    return None


def json_requested(argv):
    return "--json" in argv or any(value.startswith("--json=") for value in argv)


def valid_task_id(value):
    return isinstance(value, str) and TASK_ID_RE.fullmatch(value) is not None


def valid_synthetic_request(argv, command, expected_root):
    """Mirror only the request boundary needed by this fixture.

    The actual Delegate process remains the parser of record.  These checks are
    a second safety gate so a fixture cannot turn a malformed or incomplete
    invocation into a passing synthetic response.
    """
    if command is None or not json_requested(argv) or requested_root(argv) != expected_root:
        return False
    if command == "dispatch":
        brief = option_value(argv, "--brief")
        cwd = option_value(argv, "--cwd")
        permission = option_value(argv, "--permission")
        budget = option_value(argv, "--budget")
        provider = option_value(argv, "--provider")
        auto = "--auto" in argv
        if not brief or not Path(brief).is_absolute() or not Path(brief).is_file():
            return False
        if not cwd or not Path(cwd).is_absolute() or not Path(cwd).is_dir():
            return False
        if permission not in ("read-only", "workspace-write") or not budget:
            return False
        if auto == bool(provider):
            return False
        return auto or provider in PROVIDERS
    if command in ("status", "collect", "recheck"):
        return valid_task_id(requested_task(argv, command))
    return False


def parsed_response(result, command):
    try:
        response = json.loads(result.stdout)
    except (TypeError, ValueError):
        return None
    if not isinstance(response, dict) or response.get("command") != command:
        return None
    # A usage error or an explicitly blocked prerequisite must remain real.
    failure = response.get("failure")
    if response.get("status") == "blocked" or (
            isinstance(failure, dict)
            and (failure.get("code") == "invalid_request"
                 or failure.get("next_action") == "correct_request")):
        return None
    return response


def trusted_delegate(runtime):
    candidate = Path(runtime.get("delegate", ""))
    expected = runtime.get("delegate_sha256")
    if not candidate.is_file() or not os.access(candidate, os.X_OK):
        return False
    if expected is not None and digest(candidate) != expected:
        return False
    return True


def fixture_marker(scenario):
    return {
        "source": SYNTHETIC_EVIDENCE,
        "scenario": scenario,
        "provider_execution": "not_run",
    }


def root_id(root):
    return digest_bytes(("synthetic-root\0" + str(root)).encode())[:32]


def task_id(root, scenario):
    return digest_bytes(("synthetic-task\0" + scenario + "\0" + str(root)).encode())[:32]


def predicate(adapter, version, scenario):
    contract = f"{SYNTHETIC_EVIDENCE}\0{scenario}\0{adapter}\0{version}"
    return {
        "adapter": adapter,
        "mode": "read-only",
        "version": version,
        "sha256": digest_bytes(contract.encode()),
    }


def assessment_id(source_outcome, source_seal, target):
    identity = {
        "source_outcome_sha256": source_outcome,
        "source_seal_sha256": source_seal,
        "target_predicate": target,
    }
    canonical = json.dumps(identity, separators=(",", ":"), ensure_ascii=False).encode() + b"\n"
    return digest_bytes(canonical)


def build_state(root, scenario, provider):
    task = task_id(root, scenario)
    adapter = provider if provider in PROVIDERS else "codex:exec"
    source = predicate(adapter, "routing-source-v1", scenario)
    target = predicate(adapter, "routing-recovery-v1", scenario)
    source_outcome = digest_bytes(("synthetic-outcome\0" + task).encode())
    source_seal = digest_bytes(("synthetic-seal\0" + task).encode())
    source_evidence = digest_bytes(("synthetic-evidence\0" + task).encode())
    spec = digest_bytes(("synthetic-spec\0" + task).encode())
    meta = digest_bytes(("synthetic-meta\0" + task).encode())
    rejection = ("synthetic terminal rejection for " + scenario + "\n").encode()
    answer = ("synthetic recovered answer for " + scenario + "\n").encode()
    return {
        "schema_version": 1,
        "evidence_source": SYNTHETIC_EVIDENCE,
        "scenario": scenario,
        "root": str(root),
        "root_id": root_id(root),
        "task_id": task,
        "provider": adapter,
        "dispatch_count": 0,
        "recheck_count": 0,
        "source_outcome_sha256": source_outcome,
        "source_seal_sha256": source_seal,
        "source_evidence_sha256": source_evidence,
        "spec_sha256": spec,
        "meta_sha256": meta,
        "source_predicate": source,
        "target_predicate": target,
        "assessment_id": assessment_id(source_outcome, source_seal, target),
        "sealed_evidence": {
            "sealed": True,
            "manifest_sha256": source_evidence,
            "raw_content_returned": False,
        },
        "payload": {
            "basename": "publish.reject",
            "length": len(rejection),
            "sha256": digest_bytes(rejection),
        },
        "answer": {
            "basename": "result.txt",
            "length": len(answer),
            "sha256": digest_bytes(answer),
        },
    }


def load_state(path):
    if path.is_symlink():
        raise RuntimeError("routing state is a symlink")
    if not path.exists():
        return None
    state = read_json(path)
    if not isinstance(state, dict) or state.get("evidence_source") != SYNTHETIC_EVIDENCE:
        raise RuntimeError("invalid routing fixture state")
    return state


def common_response(state, command):
    outcome = {
        "schema_version": 1,
        "root_id": state["root_id"],
        "task_id": state["task_id"],
        "spec_sha256": state["spec_sha256"],
        "meta_sha256": state["meta_sha256"],
        "verdict": "rejected",
        "evidence_sha256": state["source_evidence_sha256"],
        "predicate": state["source_predicate"],
        "payload": state["payload"],
    }
    return {
        "schema_version": 1,
        "command": command,
        "root_id": state["root_id"],
        "task_id": state["task_id"],
        "admission": "admitted",
        "liveness": "ended",
        "publication": "rejected",
        "outcome": outcome,
        "payload": state["payload"],
        "evidence_sha256": state["source_evidence_sha256"],
        "fixture": fixture_marker(state["scenario"]),
    }


def dispatch_response(state):
    response = common_response(state, "dispatch")
    response.update(task_record="created", liveness="running",
                    publication="pending")
    response.pop("outcome")
    response.pop("payload")
    response.pop("evidence_sha256")
    return response


def terminal_response(state, command):
    response = common_response(state, command)
    response["failure"] = {
        "code": "rejected_terminal_evidence",
        "stage": "publish",
        "next_action": "recheck" if state["recheck_count"] == 0 else "stop_and_report",
    }
    return response


def recheck_response(state, fixture_root):
    response = common_response(state, "recheck")
    report = {
        "status": SCENARIOS[state["scenario"]],
        "assessment_id": state["assessment_id"],
        "source_outcome_sha256": state["source_outcome_sha256"],
        "source_seal_sha256": state["source_seal_sha256"],
        "source_evidence_sha256": state["source_evidence_sha256"],
        "source_predicate": state["source_predicate"],
        "target": state["target_predicate"],
        "report_path": str(fixture_root / "rechecks" / state["assessment_id"] / "report.json"),
    }
    if report["status"] == "recovered":
        report["answer"] = state["answer"]
        report["answer_path"] = str(fixture_root / "rechecks" / state["assessment_id"] / "result.txt")
    directory = Path(report["report_path"]).parent
    directory.mkdir(parents=True)
    if report["status"] == "recovered":
        answer = ("synthetic recovered answer for " + state["scenario"] + "\n").encode()
        with Path(report["answer_path"]).open("xb") as stream:
            stream.write(answer)
    write_json(report["report_path"], report)
    response["recheck"] = report
    response["publication"] = "rejected"
    return response


def duplicate_response(state, command, message):
    return {
        "schema_version": 1,
        "command": command,
        "root_id": state["root_id"],
        "task_id": state["task_id"],
        "admission": "unknown",
        "liveness": "undetermined",
        "publication": "unknown",
        "failure": {
            "code": "synthetic_fixture_reuse",
            "stage": command,
            "next_action": "stop_and_report",
        },
        "error": message,
        "fixture": fixture_marker(state["scenario"]),
    }


def synthetic_response(root, runtime, argv, command, normal):
    scenario = runtime.get("scenario")
    if scenario not in SCENARIOS or not trusted_delegate(runtime):
        return None
    delegate_root = os.environ.get("DELEGATE_ROOT")
    if not delegate_root:
        return None
    expected_root = Path(delegate_root).resolve()
    if not expected_root.is_absolute() or not valid_synthetic_request(argv, command, expected_root):
        return None
    if normal is None:
        return None

    state_path = root / STATE_NAME
    requested = requested_task(argv, command)
    with state_lock(state_path):
        state = load_state(state_path)
        if state is None:
            if command != "dispatch":
                return None
            state = build_state(expected_root, scenario, runtime.get("provider"))
        if state["scenario"] != scenario or state["root"] != str(expected_root):
            return None
        if command == "dispatch":
            if state["dispatch_count"] != 0:
                response, code, action = duplicate_response(
                    state, command, "synthetic dispatch already created its one task"), 2, "duplicate-dispatch"
            else:
                state["dispatch_count"] = 1
                response, code, action = dispatch_response(state), 0, "dispatch"
        elif requested != state["task_id"] or state["dispatch_count"] != 1:
            return None
        elif command in ("status", "collect"):
            response, code, action = terminal_response(state, command), 4 if command == "collect" else 0, command
        elif state["recheck_count"] != 0:
            response, code, action = duplicate_response(
                state, command, "synthetic recheck is bounded to one assessment"), 2, "duplicate-recheck"
        else:
            state["recheck_count"] = 1
            response = recheck_response(state, root)
            code = 0 if response["recheck"]["status"] == "recovered" else 4
            action = "recheck"
        write_json(state_path, state)
        append_json(root / EVENTS_NAME, {
            "source": SYNTHETIC_EVIDENCE,
            "scenario": scenario,
            "command": command,
            "action": action,
            "root": str(expected_root),
            "task_id": state["task_id"],
            "dispatch_count": state["dispatch_count"],
            "recheck_count": state["recheck_count"],
            "normal_validation_exit": normal.get("_exit_code"),
        })
    return response, code


root = Path(os.environ["DELEGATE_ROUTING_FIXTURE"])
argv = sys.argv[1:]
with (root / "calls.jsonl").open("a", encoding="utf-8") as stream:
    stream.write(json.dumps(argv) + "\n")
runtime = read_json(root / "runtime.json")

# Only this child loses provider executables. The initiating harness keeps its
# normal profile, PATH, and competing integrations, so bypasses remain observable.
try:
    result = subprocess.run([runtime["delegate"], *argv],
                            env=dict(os.environ, PATH=runtime["path"]),
                            capture_output=True, text=True, check=False)
except OSError as error:
    # A missing Delegate binary is a fixture blocker, never synthetic success.
    print("routing fixture validation unavailable: " + type(error).__name__, file=sys.stderr)
    sys.exit(1)

command = command_name(argv)
normal = parsed_response(result, command)
normal_for_fixture = None if normal is None else dict(normal, _exit_code=result.returncode)
synthetic = synthetic_response(root, runtime, argv, command, normal_for_fixture)
if synthetic is not None:
    response, code = synthetic
    response.pop("_exit_code", None)
    print(json.dumps(response))
    sys.exit(code)

# Keep model selection deterministic, but only after real parsing accepted a
# CCR discovery request. Syntax errors retain their actual error response.
if isinstance(normal, dict) and normal.get("command") == "models" \
        and normal.get("provider") == "ccr:print" \
        and normal.get("reason_code") == "discovery_unavailable" \
        and result.returncode == 2:
    normal.update(status="available", reason_code="ok", complete=True,
                  source="ccr model list", efforts=None,
                  models=[{"id": "local-review", "name": "Local review model", "efforts": None}])
    print(json.dumps(normal))
    sys.exit(0)
sys.stdout.write(result.stdout)
sys.stderr.write(result.stderr)
sys.exit(result.returncode)
