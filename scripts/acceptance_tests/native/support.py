"""Shared data-only helpers for the native acceptance unit cohorts."""

from pathlib import Path
from collections.abc import Callable, Iterable, Mapping, Sequence
from dataclasses import dataclass
import shlex
import unittest
from unittest.mock import patch

import acceptance_provider_common as provider_common
from scripts.delegate_acceptance.native import acceptance as gate
from scripts.delegate_acceptance.tasks.contracts import TaskCommand, TaskProcesses
# The fixture builds the current contract; tests assert its literal pins independently.
from scripts.delegate_acceptance.native_evidence.production_acp import (
    ACP_PREDICATE_VERSION,
    ACP_PROFILE_REVISION,
)


AGGREGATE_WRAPPER_TEST_TIMEOUT = 60


def construct_with_invalid_fields[T](constructor: Callable[..., T], fields: Mapping[str, object]) -> T:
    # Negative tests intentionally exceed the static input contract; the actual constructor validates them.
    return constructor(**fields)


def invoke_with_invalid_arguments[T](operation: Callable[..., T], arguments: Sequence[object],
                                     fields: Mapping[str, object]) -> T:
    # Negative tests intentionally exceed the static call contract; the actual operation must reject them.
    return operation(*arguments, **fields)


@dataclass(frozen=True)
class CapturedCommand:
    """A fixture's observed command capture; its enclosing test owns the files."""

    directory: Path
    result: Mapping[str, object]

    def json(self, bound: int = 8 * 1024 * 1024) -> object:
        return gate.read_json(self.directory / "stdout", bound)


def fixture_record(value: object) -> dict[str, object]:
    if not isinstance(value, dict) or not all(isinstance(key, str) for key in value):
        raise AssertionError("fixture evidence must be an object with string keys")
    return value


def fixture_list(value: object) -> list[object]:
    if not isinstance(value, list):
        raise AssertionError("fixture evidence must be a list")
    return value


def fixture_text(value: object) -> str:
    if not isinstance(value, str):
        raise AssertionError("fixture evidence must be text")
    return value


class NoCommands:
    """Pure ledger fixtures must refuse any attempted child launch or cleanup."""

    def run(self, name: str, argv: list[object], cwd: Path,
            expected: int | set[int] | None = 0, timeout: float = 15,
            stdin: bytes | None = None, *, environment: Mapping[str, str] | None = None) -> TaskCommand:
        raise AssertionError("a pure ledger fixture must not launch a command")

    def drain(self, timeout: float = 70, exclude: Iterable[object] = ()) -> Sequence[Mapping[str, object]]:
        raise AssertionError("a pure ledger fixture must not request process cleanup")


def task_owner(check: unittest.TestCase, *, processes: TaskProcesses, provider: str,
               delegate: Path, runner: Path, pueue: Path, base: Path, state: Path,
               output: Path) -> gate.NativeAcceptance:
    """Build the native state read by a task ledger, with a test-owned process boundary."""
    owner = object.__new__(gate.NativeAcceptance)
    owner.provider, owner.delegate, owner.runner, owner.pueue = provider, delegate, runner, pueue
    owner.base, owner.state, owner.output = base, state, output
    owner.models_state, owner.config = base / "models-state", base / "pueue.yml"
    owner.root_id, owner.daemon = None, None
    check.enterContext(patch.object(owner, "processes", processes, create=True))
    return owner


FAKE_NATIVE_PYTHON_SCRIPT = '''#!/bin/sh
if [ "$1" = -c ]; then
    printf '3\\n'
    exit 0
fi
case "$1" in
    *native_acceptance_evidence.py)
        if [ "${FAKE_HELPER_EXIT:-0}" -ne 0 ] && [ "$2" = aggregate ]; then
            exit "${FAKE_HELPER_EXIT}"
        fi
        if [ "$2" = list-cases ] && [ "${FAKE_LIST_EXIT:-0}" -ne 0 ]; then
            exit "$FAKE_LIST_EXIT"
        fi
        exec "$REAL_PYTHON" "$@"
        ;;
    *acceptance_native.py)
        if [ "${FAKE_READ_STDIN:-0}" -eq 1 ]; then
            cat >/dev/null
        fi
        output=
        provider=
        mode=
        scenario=
        model=
        model_set=absent
        while [ "$#" -gt 0 ]; do
            case "$1" in
                --output) output=$2; shift 2 ;;
                --provider) provider=$2; shift 2 ;;
                --permission) mode=$2; shift 2 ;;
                --scenario) scenario=$2; shift 2 ;;
                --model) model=$2; model_set=present; shift 2 ;;
                *) shift ;;
            esac
        done
        model_json=$("$REAL_PYTHON" -c 'import json, sys; print(json.dumps(sys.argv[2] if sys.argv[1] == "present" else None))' "$model_set" "$model")
        code=2
        case "${FAKE_NATIVE_RESULT:-blocked}" in
            one-failure)
                if [ "$provider" = codex:exec ] && [ "$scenario" = non-git ] && [ "$mode" = read-only ]; then
                    code=1
                fi
                ;;
            mixed)
                case "$provider" in
                    claude:print) code=0 ;;
                    codex:exec) code=1 ;;
                esac
                ;;
            timeout)
                if [ "$scenario" = timeout-resume ] || [ "$provider" = claude:print ]; then
                    code=0
                fi
                ;;
            only-timeout)
                if [ "$scenario" = timeout-resume ]; then
                    code=0
                fi
                ;;
        esac
        if [ "$code" -eq 0 ]; then
            if [ "$scenario" = timeout-resume ]; then
                tasks='{"timeout_source":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","timeout_resume":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}'
                sessions='{"timeout_source":"fixture-session","timeout_resume":"fixture-session"}'
            elif [ "$provider" = codex:review ]; then
                tasks='{"fresh":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}'
                sessions='{"fresh":"fixture-session"}'
                versions='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"1.0"}'
                runtimes='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"executable":"/fixture/provider","version":"1.0"}}'
            else
                tasks='{"fresh":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resume":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}'
                sessions='{"fresh":"fixture-session","resume":"fixture-session"}'
                versions='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"1.0","bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":"1.1"}'
                runtimes='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"executable":"/fixture/provider","version":"1.0"},"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":{"executable":"/fixture/provider","version":"1.1"}}'
            fi
            if [ "$scenario" = timeout-resume ]; then
                versions='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"1.0","bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":"1.1"}'
                runtimes='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"executable":"/fixture/provider","version":"1.0"},"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":{"executable":"/fixture/provider","version":"1.1"}}'
            fi
            production_acp=
            if [ "$provider" = opencode:run ]; then
                production_acp=',"production_acp":'"$("$REAL_PYTHON" -c 'import json, sys; sys.path[:0] = ["scripts", "."]; from scripts.acceptance_tests.native.support import production_acp_source; print(json.dumps(production_acp_source(json.loads(sys.argv[1]), json.loads(sys.argv[2]))))' "$tasks" "$sessions")"
            fi
            closure=$("$REAL_PYTHON" -c 'import json, sys; sys.path[:0] = ["scripts", "."]; from scripts.acceptance_tests.native.support import lifetime_closure_source; print(json.dumps(lifetime_closure_source(json.loads(sys.argv[1]))))' "$tasks")
            printf '{"gate":"acceptance-native","status":"passed","requested_model":%s,"requested_effort":null,"provider":"%s","mode":"%s","scenario":"%s","tasks":%s,"provider_sessions":%s,"provider_versions":%s,"provider_runtime_identities":%s,"natural_daemon_shutdown":true,"lifetime_closure":%s%s}\\n' "$model_json" "$provider" "$mode" "$scenario" "$tasks" "$sessions" "$versions" "$runtimes" "$closure" "$production_acp" >"$output/success.json"
            printf '{"requested_model":%s,"requested_effort":null,"provider":"%s","mode":"%s","scenario":"%s","provider_identity_scope":"first-admitted-task","provider_task_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","provider_executable":"/fixture/provider","provider_version":"1.0","schema_version":2,"representation":"installed-paths","installed_executables":{"delegate":"/fixture/delegate","delegate-run":"/fixture/delegate-run","pueue":"/fixture/pueue","pueued":"/fixture/pueued"}}\\n' "$model_json" "$provider" "$mode" "$scenario" >"$output/binding.json"
            printf '{"status":"removed","supervisor_shutdown_observed":true,"raw_process_captures":"removed"}\\n' >"$output/cleanup.json"
        elif [ "$code" -eq 2 ]; then
            printf '{"gate":"acceptance-native","status":"BLOCKED","requested_model":%s,"requested_effort":null,"provider":"%s","mode":"%s","scenario":"%s"}\\n' "$model_json" "$provider" "$mode" "$scenario" >"$output/failure.json"
        else
            printf '{"gate":"acceptance-native","status":"failed","requested_model":%s,"requested_effort":null,"provider":"%s","mode":"%s","scenario":"%s"}\\n' "$model_json" "$provider" "$mode" "$scenario" >"$output/failure.json"
            printf 'unresolved raw evidence\\n' >"$output/raw-provider-output"
        fi
        exit "$code"
        ;;
esac
exit 7
'''


PRODUCTION_ACP_PREDECESSORS = {"resume": "fresh", "timeout_resume": "timeout_source"}


def production_acp_source(tasks: dict[str, str], sessions: dict[str, str]) -> dict[str, object]:
    """Return a well-formed source ACP marker bound to fixture task identities."""
    turns: dict[str, dict[str, object]] = {}
    for alias, task_id in tasks.items():
        committed = alias != "timeout_source"
        predecessor = PRODUCTION_ACP_PREDECESSORS.get(alias)
        turns[alias] = {
            "task_id": task_id, "session_id": sessions[alias],
            "predecessor_task_id": tasks[predecessor] if predecessor else None,
            "profile_revision": ACP_PROFILE_REVISION, "predicate_version": ACP_PREDICATE_VERSION,
            "predicate_sha256": "7" * 64, "spec_sha256": "8" * 64, "meta_sha256": "9" * 64,
            "noninteractive": True, "interaction_journal": "absent",
            "publication": "committed" if committed else "unpublished",
            "manifest_sha256": "a" * 64 if committed else None,
            "seal_sha256": "b" * 64 if committed else None,
            "outcome_sha256": "c" * 64 if committed else None,
        }
    return {
        "schema_version": 1, "kind": "opencode-production-acp",
        "profile_revision": ACP_PROFILE_REVISION, "predicate_version": ACP_PREDICATE_VERSION,
        "tasks": turns,
    }


def lifetime_closure_fact(*, cause: str = "completed", inspection: bool = False) -> dict[str, object]:
    """Return one positive compact managed-lifetime fact."""
    identity = {"request_sha256": "d" * 64} if inspection else {"spec_sha256": "8" * 64, "meta_sha256": "9" * 64}
    return {
        "cause": cause, "scope_empty": True, "wait_result": "success", "recovered": False,
        "launch_count": 1, "registration_sha256": "a" * 64, "termination_sha256": "b" * 64,
        "wait_sha256": "c" * 64, **identity,
    }


def lifetime_closure_source(tasks: dict[str, str], *, model_discovery: str | None = None) -> dict[str, object]:
    """Return a positive compact closure proof bound to fixture task identities."""
    discovery = None if model_discovery is None else {
        "root_id": "f" * 32, "launch": "admitted", "tasks": {},
        "inspections": {model_discovery: lifetime_closure_fact(inspection=True)},
    }
    return {
        "schema_version": 1, "kind": "native-lifetime-closure",
        "ordinary": {
            "root_id": "e" * 32, "launch": "admitted", "inspections": {},
            "tasks": {task_id: lifetime_closure_fact(cause="budget" if alias == "timeout_source" else "completed")
                      for alias, task_id in sorted(tasks.items())},
        },
        "model_discovery": discovery,
    }


def adjacent_runner_bundle(root: Path) -> tuple[Path, Path]:
    """Create one artifact directory holding delegate and its managed runner."""
    bundle = root / "bundle"
    bundle.mkdir()
    delegate, runner = bundle / "delegate", bundle / "delegate-run"
    for path in (delegate, runner):
        path.write_text("#!/bin/sh\nexit 0\n")
        path.chmod(0o700)
    return delegate, runner


def physical_test_root(directory: str) -> Path:
    return Path(directory).resolve(strict=True)


def repository_root() -> Path:
    return Path(__file__).resolve().parents[3]


def fake_native_python_script() -> str:
    return FAKE_NATIVE_PYTHON_SCRIPT


def queue_oracle_owner(root: Path, runtime_only: bool, check: unittest.TestCase):
    state = root / "state"
    state.mkdir()
    runner = root / "delegate-run"
    runner.write_text("#!/bin/sh\n")
    runner.chmod(0o700)
    task = "a" * 32
    operation = object.__new__(gate.NativeAcceptance)
    operation.root_id = "b" * 32
    operation.state = state
    operation.runner = runner
    operation.tasks = {"fresh": task}
    operation.numeric_ids = {task: 7}
    check.enterContext(patch.object(operation, "processes", NoCommands(), create=True))
    operation.delegate = runner
    operation.pueue = root / "pueue"
    operation.config = root / "pueue.yml"
    operation.output = root / "output"
    operation.output.mkdir()
    operation.pueue.write_text("pueue\n")
    operation.config.write_text("config\n")
    ledger = provider_common.NativeTaskOps(
        NoCommands(), operation.delegate, operation.runner,
        operation.pueue, state.parent, state, operation.output)
    check.enterContext(patch.object(operation, "ops", ledger, create=True))
    operation.ops.expected_results = {"fresh": "Success"}
    operation.provider_executable = runner
    operation.daemon = None
    task_directory = state / "tasks" / task
    task_directory.mkdir(parents=True)
    metadata: dict[str, object] = {
        "root_id": operation.root_id,
        "task_id": task,
        "provider_executable": str(runner),
        "provider_version": "" if runtime_only else "fixture-v1",
        "effective_config": {"policy": {}},
    }
    if runtime_only:
        metadata.update({
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        })
    gate.write_json(task_directory / "meta.json", metadata)
    started: dict[str, object] | None = None
    if runtime_only:
        started = {
            "provider_executable": str(runner),
            "provider_version": "fixture-v1",
        }
    operation._record_provider_runtime_identity(task, metadata, started_evidence=started)
    return operation, task, runner


def queue_oracle(root: Path, with_inspection: bool = True,
                 runtime_only: bool = False, *, check: unittest.TestCase,
                 stub_inspection_journals: bool = True):
    operation, task, runner = queue_oracle_owner(root, runtime_only, check)
    state = operation.state
    ordinary_command = shlex.join([str(runner), "--root", str(state), task])
    rows = {"7": {
        "id": 7, "label": f"delegate:{operation.root_id}:{task}",
        "group": "default", "original_command": ordinary_command,
        "command": ordinary_command, "path": str(state),
        "status": {"Done": {"result": "Success"}},
    }}
    groups = {"default": {"status": "Running", "parallel_tasks": 1}}
    if with_inspection:
        inspection_group = gate.INSPECTION_GROUP_PREFIX + operation.root_id
        inspection_label = f"{inspection_group}-{task}"
        inspection_command = shlex.join([
            str(runner), "--inspection", "--guardian", "--supervised",
            "--root", str(state), task,
        ])
        rows["8"] = {
            "id": 8, "label": inspection_label, "group": inspection_group,
            "original_command": inspection_command, "command": inspection_command,
            "path": str(root), "status": {"Done": {"result": "Success"}},
        }
        groups[inspection_group] = {"status": "Running", "parallel_tasks": 1}
    operation.ops.bind_supervisor(operation.config)
    operation.ops.root_id = operation.root_id
    operation.ops.tasks = {"fresh": task}
    operation.ops.labels = {"fresh": f"delegate:{operation.root_id}:{task}"}
    operation.ops.numbers = {"fresh": 7}
    operation.ops.expected_results = {"fresh": "Success"}
    operation.ops.dispatch_attempts = {task}
    if with_inspection and stub_inspection_journals:
        inspection_record = {
            "task_id": task, "group": inspection_group, "label": inspection_label,
            "operation_id": task, "group_parallelism": 1,
            "historical_attempts": [], "historical_aborted": False,
            "numeric_task_id": 8,
            "inspection_exit_authority": {"mode": "queue"},
            "request": {"binding": {
                "helper_executable": str(runner),
            }},
            "result": {"reason": "eligible"},
        }
        check.enterContext(patch.object(operation.ops, "_inspection_journals", return_value={task: inspection_record}))
    return operation, {"tasks": rows, "groups": groups}


def acceptance_task_ops(operation: gate.NativeAcceptance, root: Path):
    operation.base = root
    operation.models_state = root / "models-state"
    operation.scenario = "read-only"
    # The task ledger reads its runner wait from the owner's provider case.
    vars(operation).setdefault("provider", "codex:exec")
    return gate.NativeAcceptanceTaskOps(operation)
