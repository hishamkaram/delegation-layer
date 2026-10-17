"""Owned package and provider fixture installation."""

from scripts.delegate_acceptance.private_cli.fixture_records import FixtureInstallation


import os
from pathlib import Path
import tempfile


from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.configuration import EXTERNAL_INSPECTION_HANDOFF_SCENARIO
from scripts.delegate_acceptance.private_cli.configuration import PRIVATE_ACCEPTANCE_PREFIX
from scripts.delegate_acceptance.private_cli.configuration import SUPERVISOR_UPGRADE_RECOVERY_SCENARIO
from scripts.delegate_acceptance.private_cli.installation import install_fixture
from scripts.delegate_acceptance.private_cli.installation import install_package_layout
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
    lifetime.private_config = lifetime.base / "state" / ".supervisor" / "pueue.yml"
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
    state = lifetime.ownership.create("private CLI state", lifetime.base / "state", lifetime.base).directory
    waiter_directory = lifetime.ownership.create(
        "provider waiter directory", lifetime.base / "provider-waiters", lifetime.base
    ).directory
    if args.scenario != EXTERNAL_INSPECTION_HANDOFF_SCENARIO:
        lifetime.managed_state_roots.append((state, lifetime.attempted_task_ids))
    upgraded_install, upgraded_bin, lifetime.upgraded_libexec = install_package_layout(
        lifetime.ownership, upgraded_delegate_binary, upgraded_runner_binary, upgraded_root, True
    )
    response_directory.mkdir(mode=0o700)
    old_daemon_marker = lifetime.base / "old-daemon-starts"
    upgraded_daemon_marker = lifetime.base / "upgraded-daemon-starts"
    install_supervisor_wrappers(args, lifetime, old_daemon_marker, pueue_binary, pueued_binary, upgraded_daemon_marker)
    install_pi_fixture(provider_bin, lifetime.gate, waiter_directory, lease_nonce)

    brief = configure_private_environment(home, lifetime, provider_bin)
    return FixtureInstallation(
        delegate_binary,
        runner_binary,
        upgraded_delegate_binary,
        upgraded_runner_binary,
        pueue_binary,
        pueued_binary,
        workspace,
        provider_bin,
        brief,
        upgraded_install,
        old_daemon_marker,
        upgraded_daemon_marker,
        state,
        waiter_directory,
        lease_nonce,
    )


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


def configure_private_environment(home, lifetime, provider_bin):
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
    return brief
