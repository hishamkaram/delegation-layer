"""Owned package and provider fixture installation."""

from scripts.delegate_acceptance.private_cli.fixture_records import FixtureInstallation


import os
from pathlib import Path
import tempfile


from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_NOEXEC_STATE_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import PRIVATE_ACCEPTANCE_PREFIX
from scripts.delegate_acceptance.private_cli.configuration import SUPERVISOR_UPGRADE_RECOVERY_SCENARIO
from scripts.delegate_acceptance.private_cli.installation import install_fixture
from scripts.delegate_acceptance.private_cli.installation import install_package_layout
from scripts.delegate_acceptance.private_cli.noexec_state import require_execution_refused
from scripts.delegate_acceptance.private_cli.fixtures import install_pi_fixture
from scripts.delegate_acceptance.private_cli.wrappers import install_wrapper


def prepare_fixture(args, lease_nonce, legacy_mode, lifetime, response_directory, run_receipt):
    if not os.access("/bin/ps", os.X_OK):
        raise AcceptanceBlocked("shutdown-probe-unavailable")
    try:
        delegate_binary = args.delegate.resolve(strict=True)
        runner_binary = args.runner.resolve(strict=True)
        upgraded_delegate_binary = (args.upgraded_delegate or args.delegate).resolve(strict=True)
        upgraded_runner_binary = (args.upgraded_runner or args.runner).resolve(strict=True)
        pueue_binary = args.pueue.resolve(strict=True)
        pueued_binary = args.pueued.resolve(strict=True)
    except OSError as error:
        raise AcceptanceBlocked("required-binary-unavailable") from error
    run_receipt.phase("fixture_setup")
    acceptance_parent = Path.home().resolve(strict=True)
    base_path = Path(tempfile.mkdtemp(prefix=PRIVATE_ACCEPTANCE_PREFIX, dir=str(acceptance_parent))).resolve(
        strict=True
    )
    lifetime.base_token = lifetime.ownership.adopt_created("private CLI acceptance root", base_path, acceptance_parent)
    lifetime.base = lifetime.base_token.directory
    lifetime.capture_token = lifetime.ownership.create(
        "private control captures", lifetime.base / "private-control-captures", lifetime.base
    )
    state_parent, temporary_root = prepare_state_parent(args, lifetime, run_receipt)
    lifetime.private_config = state_parent / "state" / ".supervisor" / "pueue.yml"
    lifetime.gate = lifetime.base / "blocker-gate"
    waiter_directory = lifetime.base / "provider-waiters"
    old_root = lifetime.old_layout_ownership.create("old package root", lifetime.base / "old", lifetime.base).directory
    lifetime.old_install, old_bin, lifetime.old_libexec = install_package_layout(
        lifetime.old_layout_ownership, delegate_binary, runner_binary, old_root, not legacy_mode
    )
    lifetime.old_layout_owned = lifetime.old_layout_ownership.transfer()
    upgraded_root = lifetime.ownership.create(
        "upgraded package root", lifetime.base / "upgraded", lifetime.base
    ).directory
    provider_bin = lifetime.ownership.create(
        "provider binaries", lifetime.base / "provider-bin", lifetime.base
    ).directory
    home = lifetime.ownership.create("private CLI home", lifetime.base / "home", lifetime.base).directory
    workspace = lifetime.ownership.create("private CLI workspace", lifetime.base / "workspace", lifetime.base).directory
    state = lifetime.ownership.create("private CLI state", state_parent / "state", state_parent).directory
    waiter_directory = lifetime.ownership.create(
        "provider waiter directory", lifetime.base / "provider-waiters", lifetime.base
    ).directory
    if args.scenario not in (EXTERNAL_INSPECTION_HANDOFF_SCENARIO, EXTERNAL_NOEXEC_STATE_SCENARIO):
        lifetime.managed_state_roots.append((state, lifetime.attempted_task_ids))
    upgraded_install, upgraded_bin, lifetime.upgraded_libexec = install_package_layout(
        lifetime.ownership, upgraded_delegate_binary, upgraded_runner_binary, upgraded_root, True
    )
    response_directory.mkdir(mode=0o700)
    old_daemon_marker = lifetime.base / "old-daemon-starts"
    upgraded_daemon_marker = lifetime.base / "upgraded-daemon-starts"
    install_supervisor_wrappers(args, lifetime, old_daemon_marker, pueue_binary, pueued_binary, upgraded_daemon_marker)
    install_pi_fixture(provider_bin, lifetime.gate, waiter_directory, lease_nonce)

    brief = configure_private_environment(home, lifetime, provider_bin, temporary_root)
    return FixtureInstallation(
        delegate_binary=delegate_binary,
        runner_binary=runner_binary,
        upgraded_delegate_binary=upgraded_delegate_binary,
        upgraded_runner_binary=upgraded_runner_binary,
        pueue_binary=pueue_binary,
        pueued_binary=pueued_binary,
        workspace=workspace,
        provider_bin=provider_bin,
        brief=brief,
        upgraded_install=upgraded_install,
        old_daemon_marker=old_daemon_marker,
        upgraded_daemon_marker=upgraded_daemon_marker,
        state=state,
        waiter_directory=waiter_directory,
        lease_nonce=lease_nonce,
    )


def prepare_state_parent(args, lifetime, run_receipt):
    """Return where the state root lives and the execution temporary root, if the scenario needs one.

    A noexec state parent belongs to the caller. The harness owns, and later
    removes, only the container it creates there and the temporary root it
    creates beneath its own acceptance root.
    """
    if args.noexec_state_parent is None:
        return lifetime.base, None
    try:
        parent = args.noexec_state_parent.resolve(strict=True)
    except OSError as error:
        raise AcceptanceBlocked("noexec-state-parent-unavailable") from error
    temporary_root = lifetime.ownership.create(
        "execution temporary root", lifetime.base / "tmp", lifetime.base
    ).directory
    container = lifetime.ownership.create(
        "noexec private CLI state container", parent / lifetime.base.name, parent
    ).directory
    run_receipt.add_details(noexec_state={"state_container": str(container)})
    probe = require_execution_refused(container, temporary_root)
    run_receipt.add_details(noexec_state={"state_container": str(container), "probe": probe})
    return container, temporary_root


def install_supervisor_wrappers(args, lifetime, old_daemon_marker, pueue_binary, pueued_binary, upgraded_daemon_marker):
    if args.scenario == SUPERVISOR_UPGRADE_RECOVERY_SCENARIO:
        for destination in (lifetime.old_libexec, lifetime.upgraded_libexec):
            install_fixture(pueue_binary, destination / "pueue")
            install_fixture(pueued_binary, destination / "pueued")
    else:
        install_wrapper(
            lifetime.old_libexec / "pueue", pueue_binary, lifetime.base / "old-client-starts", "old-install", "client"
        )
        install_wrapper(lifetime.old_libexec / "pueued", pueued_binary, old_daemon_marker, "old-install", "daemon")
        install_wrapper(
            lifetime.upgraded_libexec / "pueue",
            pueue_binary,
            lifetime.base / "upgraded-client-starts",
            "upgraded-install",
            "client",
        )
        install_wrapper(
            lifetime.upgraded_libexec / "pueued", pueued_binary, upgraded_daemon_marker, "upgraded-install", "daemon"
        )


def configure_private_environment(home, lifetime, provider_bin, temporary_root):
    brief = lifetime.base / "brief.md"
    brief.write_text("Reply with the requested acceptance sentence.\n")
    brief.chmod(0o600)
    lifetime.env.update(
        {
            "HOME": str(home),
            "PATH": str(provider_bin)
            + os.pathsep
            + str(lifetime.old_install)
            + os.pathsep
            + str(lifetime.old_libexec)
            + os.pathsep
            + "/usr/bin:/bin",
            "XDG_CONFIG_HOME": str(home / ".config"),
            "XDG_DATA_HOME": str(home / ".local" / "share"),
            "LANG": "C.UTF-8",
            "LC_ALL": "C.UTF-8",
        }
    )
    if temporary_root is not None:
        lifetime.env["TMPDIR"] = str(temporary_root)
    return brief
