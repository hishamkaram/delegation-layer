"""Shared data-only helpers for the native acceptance unit cohorts."""

from pathlib import Path
import shlex

import acceptance_provider_common as provider_common
from scripts.delegate_acceptance.native import acceptance as gate


AGGREGATE_WRAPPER_TEST_TIMEOUT = 60


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
                runtimes='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"executable":"/fixture/provider","version":"1.0","sha256":"1111111111111111111111111111111111111111111111111111111111111111"}}'
            else
                tasks='{"fresh":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","resume":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}'
                sessions='{"fresh":"fixture-session","resume":"fixture-session"}'
                versions='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"1.0","bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":"1.1"}'
                runtimes='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"executable":"/fixture/provider","version":"1.0","sha256":"1111111111111111111111111111111111111111111111111111111111111111"},"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":{"executable":"/fixture/provider","version":"1.1","sha256":"6666666666666666666666666666666666666666666666666666666666666666"}}'
            fi
            if [ "$scenario" = timeout-resume ]; then
                versions='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":"1.0","bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":"1.1"}'
                runtimes='{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa":{"executable":"/fixture/provider","version":"1.0","sha256":"1111111111111111111111111111111111111111111111111111111111111111"},"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb":{"executable":"/fixture/provider","version":"1.1","sha256":"6666666666666666666666666666666666666666666666666666666666666666"}}'
            fi
            printf '{"gate":"acceptance-native","status":"passed","requested_model":%s,"requested_effort":null,"provider":"%s","mode":"%s","scenario":"%s","tasks":%s,"provider_sessions":%s,"provider_versions":%s,"provider_runtime_identities":%s,"natural_daemon_shutdown":true}\\n' "$model_json" "$provider" "$mode" "$scenario" "$tasks" "$sessions" "$versions" "$runtimes" >"$output/success.json"
            printf '{"requested_model":%s,"requested_effort":null,"provider":"%s","mode":"%s","scenario":"%s","provider_identity_scope":"first-admitted-task","provider_task_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","provider_executable":"/fixture/provider","provider_version":"1.0","provider_sha256":"1111111111111111111111111111111111111111111111111111111111111111","delegate_sha256":"2222222222222222222222222222222222222222222222222222222222222222","runner_sha256":"3333333333333333333333333333333333333333333333333333333333333333","pueue_sha256":"4444444444444444444444444444444444444444444444444444444444444444","pueued_sha256":"5555555555555555555555555555555555555555555555555555555555555555"}\\n' "$model_json" "$provider" "$mode" "$scenario" >"$output/binding.json"
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


def physical_test_root(directory: str) -> Path:
    return Path(directory).resolve(strict=True)


def repository_root() -> Path:
    return Path(__file__).resolve().parents[3]


def fake_native_python_script() -> str:
    return FAKE_NATIVE_PYTHON_SCRIPT


def queue_oracle_owner(root: Path, runtime_only: bool):
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
    operation.processes = object()
    operation.delegate = runner
    operation.pueue = root / "pueue"
    operation.config = root / "pueue.yml"
    operation.output = root / "output"
    operation.output.mkdir()
    operation.pueue.write_text("pueue\n")
    operation.config.write_text("config\n")
    operation.ops = provider_common.NativeTaskOps(
        operation.processes, operation.delegate, operation.runner,
        operation.pueue, state.parent, state, operation.output)
    operation.ops.expected_results = {"fresh": "Success"}
    operation.provider_executable = runner
    operation.daemon = None
    task_directory = state / "tasks" / task
    task_directory.mkdir(parents=True)
    metadata = {
        "root_id": operation.root_id,
        "task_id": task,
        "provider_executable": str(runner),
        "provider_version": "" if runtime_only else "fixture-v1",
        "effective_config": {"policy": {"runtime_sha256": gate.digest(runner)}},
    }
    if runtime_only:
        metadata.update({
            "runtime_preflight_sha256": "a" * 64,
            "environment_recorded": True,
        })
    gate.write_json(task_directory / "meta.json", metadata)
    started = None
    if runtime_only:
        started = {
            "provider_executable": str(runner),
            "provider_version": "fixture-v1",
            "provider_sha256": gate.digest(runner),
        }
    operation._record_provider_runtime_identity(task, metadata, started_evidence=started)
    return operation, task, runner


def queue_oracle(root: Path, with_inspection: bool = True,
                 runtime_only: bool = False):
    operation, task, runner = queue_oracle_owner(root, runtime_only)
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
    if with_inspection:
        inspection_record = {
            "task_id": task, "group": inspection_group, "label": inspection_label,
            "operation_id": task, "group_parallelism": 1,
            "historical_attempts": [], "historical_aborted": False,
            "numeric_task_id": 8,
            "inspection_exit_authority": {"mode": "queue"},
            "request": {"binding": {
                "helper_executable": str(runner),
                "helper_sha256": gate.digest(runner),
            }},
            "result": {"reason": "eligible"},
        }
        operation.ops._inspection_journals = lambda *_args: {task: inspection_record}
    return operation, {"tasks": rows, "groups": groups}


def acceptance_task_ops(operation: gate.NativeAcceptance, root: Path):
    operation.base = root
    operation.models_state = root / "models-state"
    operation.scenario = "read-only"
    return gate.NativeAcceptanceTaskOps(operation)
