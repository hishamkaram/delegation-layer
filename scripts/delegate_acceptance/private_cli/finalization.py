"""Fixture release, managed closure, shutdown, and exact owned-root finalization."""

from scripts.delegate_acceptance.evidence.validation import require


from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.delegate_acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.delegate_acceptance.private_cli.capture import reap_captured_process
from scripts.delegate_acceptance.private_cli.root_cleanup import remove_private_acceptance_root
from scripts.delegate_acceptance.private_cli.supervisor import shutdown_private_supervisor


def finalize_fixture(args, failure, lifetime, run_receipt, success):
    cleanup_errors = []
    run_receipt.phase("cleanup")
    current_handoff_binding, primary_closure_ready = release_fixture_workers(args, cleanup_errors, lifetime, success)
    shutdown_supplemental_supervisors(cleanup_errors, lifetime)
    shutdown_primary_supervisor(args, cleanup_errors, current_handoff_binding, lifetime, primary_closure_ready, success)
    if lifetime.inspection_process is not None:
        _, _, reaped, retry_error = reap_captured_process(lifetime.inspection_process, 5)
        if reaped:
            lifetime.inspection_process = None
        if retry_error is not None:
            cleanup_errors.append(retry_error)
    if lifetime.inspection_process is not None:
        cleanup_errors.append(RuntimeError("fixture process remained live after bounded cleanup"))
    if cleanup_errors:
        success = False
        print(
            "Retained private acceptance state after fixture process cleanup failure: "
            + "; ".join(str(error) for error in cleanup_errors),
            flush=True,
        )
    success, root_cleanup = finish_root_cleanup(cleanup_errors, failure, lifetime, success)
    cleanup_evidence = {
        "status": (
            "failed"
            if cleanup_errors
            else "passed"
            if root_cleanup["status"] == "removed"
            else "not-required"
            if lifetime.base is None
            else "retained"
        ),
        "private_root": root_cleanup,
        "pending_supervisor_count": len(lifetime.supervisor_cleanups),
    }
    cleanup_failure = None
    if cleanup_errors and failure is None:
        cleanup_failure = RuntimeError(
            "private acceptance cleanup failed: " + "; ".join(str(error) for error in cleanup_errors)
        )
    receipt_error = failure or cleanup_failure
    result = (
        "PASS"
        if success and receipt_error is None and not cleanup_errors
        else "BLOCKED"
        if isinstance(receipt_error, AcceptanceBlocked) and not cleanup_errors
        else "FAIL"
    )
    run_receipt.write(result, cleanup_evidence, receipt_error)
    if result == "PASS":
        print("PASS private CLI acceptance: " + str(args.output))
    elif lifetime.base is not None and root_cleanup["status"] == "retained":
        print("Retained private acceptance state for diagnosis: " + str(lifetime.base))
    if cleanup_failure is not None:
        raise cleanup_failure


def release_fixture_workers(args, cleanup_errors, lifetime, success):
    primary_closure_ready = True
    if lifetime.release_gate is not None:
        try:
            lifetime.release_gate.release()
        except BaseException as cleanup_error:
            cleanup_errors.append(RuntimeError("could not release the locked fixture gate: " + str(cleanup_error)))
    elif lifetime.gate is not None and lifetime.base is not None:
        try:
            lifetime.gate.touch(mode=0o600, exist_ok=True)
        except BaseException as cleanup_error:
            cleanup_errors.append(RuntimeError("could not release the fixture gate: " + str(cleanup_error)))
    current_handoff_binding = (
        lifetime.handoff_cleanup_bindings[-1]
        if args.scenario == EXTERNAL_INSPECTION_HANDOFF_SCENARIO and lifetime.handoff_cleanup_bindings
        else None
    )
    if current_handoff_binding is not None:
        closure_state = current_handoff_binding["state"]
        closure_task_ids = tuple(current_handoff_binding["task_ids"])
        closure_config = current_handoff_binding["config"]
    else:
        closure_state = lifetime.managed_state_roots[0][0] if lifetime.managed_state_roots else None
        closure_task_ids = tuple(lifetime.managed_state_roots[0][1]) if lifetime.managed_state_roots else ()
        closure_config = lifetime.private_config
    if (
        not success
        and closure_config is not None
        and closure_config.is_file()
        and (current_handoff_binding is not None or lifetime.managed_state_roots)
    ):
        try:
            await_managed_lifetime_closure(closure_state, closure_task_ids, timeout=45)
        except BaseException as cleanup_error:
            primary_closure_ready = False
            cleanup_errors.append(
                RuntimeError("managed workers did not close after fixture release: " + str(cleanup_error))
            )
    if lifetime.inspection_process is not None:
        _, _, reaped, process_cleanup_error = reap_captured_process(lifetime.inspection_process, 45)
        if reaped:
            lifetime.inspection_process = None
        if process_cleanup_error is not None:
            cleanup_errors.append(process_cleanup_error)
    return current_handoff_binding, primary_closure_ready


def shutdown_supplemental_supervisors(cleanup_errors, lifetime):
    for pending in tuple(lifetime.supervisor_cleanups):
        if pending["config"].is_file():
            try:
                shutdown_private_supervisor(
                    pending["client"],
                    pending["config"],
                    pending["env"],
                    state=pending["state"],
                    task_ids=tuple(pending.get("task_ids", ())),
                    capture_root=lifetime.base_token,
                )
                lifetime.supervisor_cleanups.remove(pending)
            except BaseException as cleanup_error:
                cleanup_errors.append(
                    RuntimeError("packaged default supervisor shutdown remained unverified: " + str(cleanup_error))
                )


def shutdown_primary_supervisor(
    args, cleanup_errors, current_handoff_binding, lifetime, primary_closure_ready, success
):
    if current_handoff_binding is not None:
        cleanup_client = current_handoff_binding["client"]
        cleanup_config = current_handoff_binding["config"]
        cleanup_env = current_handoff_binding["env"]
        cleanup_state = current_handoff_binding["state"]
        cleanup_task_ids = tuple(current_handoff_binding["task_ids"])
    else:
        cleanup_config = lifetime.private_config
        cleanup_env = lifetime.env
        cleanup_state = (
            lifetime.private_config.parent.parent
            if lifetime.private_config is not None and lifetime.private_config.is_file()
            else None
        )
        cleanup_task_ids = tuple(lifetime.attempted_task_ids)
        cleanup_client = next(
            (
                candidate
                for candidate in (
                    (lifetime.upgraded_libexec / "pueue" if lifetime.upgraded_libexec is not None else None),
                    (lifetime.old_libexec / "pueue" if lifetime.old_libexec is not None else None),
                    args.pueue,
                )
                if candidate is not None and candidate.is_file()
            ),
            None,
        )
    if (
        (not success or cleanup_errors)
        and primary_closure_ready
        and cleanup_config is not None
        and cleanup_config.is_file()
    ):
        client = cleanup_client
        if client is not None:
            try:
                shutdown_private_supervisor(
                    client,
                    cleanup_config,
                    cleanup_env,
                    state=cleanup_state,
                    task_ids=cleanup_task_ids,
                    capture_root=lifetime.base_token,
                )
            except BaseException as cleanup_error:
                cleanup_errors.append(
                    RuntimeError("primary private supervisor shutdown remained unverified: " + str(cleanup_error))
                )
                print("Retained private acceptance state after unproven shutdown: " + str(cleanup_error), flush=True)


def finish_root_cleanup(cleanup_errors, failure, lifetime, success):
    root_cleanup = {"status": "not-created", "verified": lifetime.base is None}
    if success and failure is None and not cleanup_errors and lifetime.base is not None:
        try:
            main_owned = lifetime.ownership.transfer()
            require(
                any(entry is lifetime.base_token for entry in main_owned),
                "private acceptance root token was lost from ownership sequence",
            )
            owned_directories = tuple(
                [lifetime.base_token, *lifetime.old_layout_owned]
                + [entry for entry in main_owned if entry is not lifetime.base_token]
            )
            remove_private_acceptance_root(
                lifetime.base_token, owned_directories, managed_state_roots=tuple(lifetime.managed_state_roots)
            )
            root_cleanup = {"status": "removed", "verified": True}
        except BaseException as cleanup_error:
            cleanup_errors.append(RuntimeError("private acceptance root removal failed: " + str(cleanup_error)))
            success = False
    if lifetime.base is not None and root_cleanup["status"] != "removed":
        retained = lifetime.base.is_dir() and not lifetime.base.is_symlink()
        root_cleanup = {"status": "retained" if retained else "missing", "verified": retained}
        if not retained:
            cleanup_errors.append(RuntimeError("private acceptance root was not retained after failure"))
    return success, root_cleanup
