"""Initial task completion and supervisor launch provenance observations."""

from scripts.acceptance.private_cli.fixture_records import InitialObservation


import json
import os
import uuid


from scripts.acceptance.private_cli.configuration import COLLECT_TIMEOUT_SECONDS
from scripts.acceptance.private_cli.configuration import COLLECT_WATCH
from scripts.acceptance.private_cli.configuration import TASK_BUDGET
from scripts.acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.acceptance.private_cli.commands import run_json


from scripts.acceptance.private_cli.packaged_defaults import verify_packaged_default_path


def prepare_initial_observations(args, legacy_mode, lifetime, response_directory, run_receipt, fixture):
    initial_pair_directory = None
    dispatch, task_id = dispatch_initial_task(args, fixture, lifetime, response_directory, run_receipt)
    (initial_pair_directory, initial_daemon, identity_path, daemon_identity, upgraded_daemon) = (
        read_initial_provenance(fixture, legacy_mode, lifetime, task_id)
    )
    primary_client = lifetime.old_libexec / "pueue" if legacy_mode else initial_pair_directory / "pueue"
    packaged_evidence = verify_packaged_default_path(
        owner=lifetime.ownership,
        base=lifetime.base,
        sources=dict(fixture.packaged_sources),
        provider_bin=fixture.provider_bin,
        brief=fixture.brief,
        primary_client=primary_client,
        primary_config=lifetime.private_config,
        primary_env=lifetime.env,
        response_directory=response_directory,
        supervisor_cleanups=lifetime.supervisor_cleanups,
        run_receipt=run_receipt,
        managed_state_roots=lifetime.managed_state_roots,
        capture_root=lifetime.capture_token,
        disposal=lifetime.disposal,
    )

    inspection_group = "delegation-inspection-" + dispatch["root_id"]
    inspection_prefix = inspection_group + "-"
    models_args = [
        "--root",
        str(fixture.state),
        "models",
        "--provider",
        "pi:json",
        "--cwd",
        str(fixture.workspace),
        "--json",
    ]
    if args.runtime_only:
        if (fixture.state / "inspection-group").exists():
            raise RuntimeError("runtime-only dispatch created a legacy group journal")
    else:
        initial_models = run_json(
            lifetime.old_install / "delegate",
            lifetime.env,
            models_args,
            receipt=response_directory / "models-initial.json",
            phase="inspection-worker",
            capture_root=lifetime.capture_token,
        )
        if initial_models.get("status") != "available" or [
            model.get("id") for model in initial_models.get("models", [])
        ] != ["openai/gpt-4.1"]:
            raise RuntimeError("legacy CLI could not complete its initial model inspection")
    return InitialObservation(
        dispatch=dispatch,
        task_id=task_id,
        initial_pair_directory=initial_pair_directory,
        initial_daemon=initial_daemon,
        identity_path=identity_path,
        daemon_identity=daemon_identity,
        upgraded_daemon=upgraded_daemon,
        packaged_evidence=packaged_evidence,
        inspection_group=inspection_group,
        inspection_prefix=inspection_prefix,
        models_args=models_args,
    )


def dispatch_initial_task(args, fixture, lifetime, response_directory, run_receipt):
    if args.parallel_tasks > 1:
        # PATH is intentionally retained by the control environment;
        # arbitrary ambient keys are correctly filtered out by Delegate.
        lifetime.env["PATH"] += os.pathsep + os.pathsep.join(["/delegate-acceptance-unused"] * 256)

    run_receipt.phase("initial_dispatch")
    task_id = uuid.uuid4().hex
    lifetime.attempted_task_ids.append(task_id)
    dispatch = run_json(
        lifetime.old_install / "delegate",
        lifetime.env,
        [
            "--root",
            str(fixture.state),
            "dispatch",
            "--provider",
            "pi:json",
            "--brief",
            str(fixture.brief),
            "--cwd",
            str(fixture.workspace),
            "--id",
            task_id,
            "--permission",
            "read-only",
            "--budget",
            TASK_BUDGET,
            "--json",
        ],
        receipt=response_directory / "dispatch-initial.json",
        phase="ordinary-dispatch",
        capture_root=lifetime.capture_token,
    )
    if dispatch.get("task_id") != task_id or ("task_record" in dispatch and dispatch.get("task_record") != "created"):
        raise RuntimeError(
            "dispatch did not durably create the requested task: response=%r expected_id=%s" % (dispatch, task_id)
        )
    if dispatch.get("admission") != "admitted":
        raise RuntimeError("dispatch did not admit the bounded task")

    first_status = run_json(
        lifetime.old_install / "delegate",
        lifetime.env,
        ["--root", str(fixture.state), "status", task_id, "--json"],
        receipt=response_directory / "status-initial.json",
        phase="ordinary-observation",
        capture_root=lifetime.capture_token,
    )
    if first_status.get("task_id") != task_id:
        raise RuntimeError("status did not observe the dispatched task")

    collected = run_json(
        lifetime.old_install / "delegate",
        lifetime.env,
        ["--root", str(fixture.state), "collect", task_id, "--watch", COLLECT_WATCH, "--json"],
        timeout=COLLECT_TIMEOUT_SECONDS,
        receipt=response_directory / "collect-initial.json",
        phase="ordinary-collection",
        capture_root=lifetime.capture_token,
    )
    if collected.get("outcome", {}).get("verdict") != "committed":
        raise RuntimeError("delegate did not publish the successful fixture result")
    run_receipt.stage("initial-private-task-completed")
    return dispatch, task_id


def read_initial_provenance(fixture, legacy_mode, lifetime, task_id):
    initial_pair_directory = None
    task_dir = fixture.state / "tasks" / task_id
    if not lifetime.private_config.is_file():
        raise RuntimeError("delegate did not create its private supervisor config")
    first_meta = json.loads((task_dir / "meta.json").read_text())
    if legacy_mode:
        initial_daemon = lifetime.old_libexec / "pueued"
        if str(lifetime.old_libexec / "pueue") not in json.dumps(first_meta) or str(initial_daemon) not in json.dumps(
            first_meta
        ):
            raise RuntimeError("legacy task did not bind the supervisor pair shipped in private libexec")
    else:
        initial_pair_directory = assert_state_supervisor_pair(fixture.state, lifetime.old_libexec)
        initial_daemon = initial_pair_directory / "pueued"
        if str(initial_pair_directory / "pueue") not in json.dumps(first_meta) or str(initial_daemon) not in json.dumps(
            first_meta
        ):
            raise RuntimeError("task did not bind the selected installed supervisor pair")
    outcome = json.loads((task_dir / "outcome.json").read_text())
    if outcome.get("verdict") != "committed":
        raise RuntimeError("durable outcome does not confirm success")

    identity_path = fixture.state / ".supervisor" / "daemon.identity.json"
    daemon_identity = identity_path.read_bytes()
    old_identity = json.loads(daemon_identity)
    upgraded_daemon = lifetime.upgraded_libexec / "pueued"
    if old_identity.get("executable") != str(initial_daemon):
        raise RuntimeError("initial launch provenance does not name its selected private daemon")
    if old_identity.get("executable") == str(upgraded_daemon):
        raise RuntimeError("acceptance layouts do not select independent daemon installations")
    if not legacy_mode and (old_identity.get("executable_identity") != "installed-path-v1"
                            or old_identity.get("sha256")):
        raise RuntimeError("current daemon identity did not record installed-path selection")
    if not fixture.old_daemon_marker.is_file() or fixture.old_daemon_marker.read_text().splitlines() != ["started"]:
        raise RuntimeError("initial CLI did not start exactly one old-layout daemon")
    return (initial_pair_directory, initial_daemon, identity_path, daemon_identity, upgraded_daemon)
