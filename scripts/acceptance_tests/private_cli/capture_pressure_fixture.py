"""Finite descriptor-pressure control-process fixture; importing starts no work."""


import concurrent.futures
import errno
import json
import os
from pathlib import Path
import resource
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[3]))
from scripts import acceptance_private_cli as private_cli


def fd_count(path):
    return len(os.listdir(path))


def main():
    old_soft, old_hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    if old_hard != resource.RLIM_INFINITY and old_hard < 256:
        raise RuntimeError("fixture hard descriptor limit is below 256")
    resource.setrlimit(resource.RLIMIT_NOFILE, (256, old_hard))
    pins = []
    capture_owner = None
    try:
        with tempfile.TemporaryDirectory(prefix="delegate-low-fd-") as temporary:
            root = Path(temporary)
            capture_owner = private_cli.DirectoryOwner()
            capture_root = capture_owner.create(
                "private control capture root", root / "capture-root", root,
            )
            client, config, command_env, fd_directory, held_fd_count, pipe_probe_errno = _prepare_pressure_control(pins, root)
            metrics = {}
            snapshot = private_cli.pueue_snapshot(
                client, config, command_env, metrics=metrics, phase="fd-probe",
                capture_root=capture_root,
            )
            control_output = private_cli.pueue_command(
                client, config, command_env, ["shutdown"], timeout=5, phase="fd-probe",
                capture_root=capture_root,
            )

            startup_watchdog_seconds, child_watchdog_seconds, markers_before_release, pending_before_release, markers_after_release, exit_codes, outputs = _probe_parallel_capture(capture_root, command_env, root)
            print(json.dumps({
                "soft_limit": resource.getrlimit(resource.RLIMIT_NOFILE)[0],
                "pinned_fds": len(pins[:201]),
                "held_fd_count": held_fd_count,
                "pipe_probe_errno": pipe_probe_errno,
                "snapshot": snapshot,
                "status_bytes": metrics["status_bytes"],
                "control_output": control_output,
                "parallel_startup_watchdog_seconds": startup_watchdog_seconds,
                "parallel_child_watchdog_seconds": child_watchdog_seconds,
                "parallel_markers_before_release": len(markers_before_release),
                "parallel_pending_before_release": pending_before_release,
                "parallel_markers_after_release": len(markers_after_release),
                "parallel_exit_codes": exit_codes,
                "parallel_outputs": outputs,
            }))
            capture_owner.rollback()
            capture_owner = None
    finally:
        if capture_owner is not None:
            try:
                capture_owner.release()
            except RuntimeError:
                # TemporaryDirectory has already removed the parent on an
                # exceptional probe path; release still consumes the pinned FDs.
                pass
        for descriptor in reversed(pins):
            try:
                os.close(descriptor)
            except OSError:
                pass
        resource.setrlimit(resource.RLIMIT_NOFILE, (old_soft, old_hard))




def _probe_parallel_capture(capture_root, command_env, root):
    marker_directory = root / "parallel-markers"
    marker_directory.mkdir()
    release_path = root / "parallel-release"
    startup_watchdog_seconds = 30
    child_watchdog_seconds = 60
    parallel_child = (
        "import sys,time\n"
        "from pathlib import Path\n"
        "directory=Path(sys.argv[1]); release=Path(sys.argv[2])\n"
        "(directory / str(__import__('os').getpid())).touch()\n"
        "deadline=time.monotonic()+60\n"
        "while not release.exists() and time.monotonic() < deadline:\n"
        " time.sleep(0.005)\n"
        "if not release.exists():\n"
        " raise SystemExit(4)\n"
        "print('delegate-short-capture')\n"
    )

    def capture(index):
        result = private_cli.run_captured_command(
            [str(sys.executable), "-c", parallel_child,
             str(marker_directory), str(release_path)],
            command_env, child_watchdog_seconds, capture_root=capture_root,
        )
        return (result.returncode,
                result.stdout.decode("utf-8").strip(),
                result.stderr.decode("utf-8").strip())

    def marker_names():
        with private_cli.CAPTURE_START_LOCK:
            return frozenset(
                path.name for path in marker_directory.iterdir() if path.is_file()
            )

    def publish_release():
        with private_cli.CAPTURE_START_LOCK:
            release_path.write_text("release\n")

    markers_before_release, pending_before_release, markers_after_release, exit_codes, outputs = _join_parallel_captures(capture, marker_names, publish_release, startup_watchdog_seconds)
    return startup_watchdog_seconds, child_watchdog_seconds, markers_before_release, pending_before_release, markers_after_release, exit_codes, outputs


def _prepare_pressure_control(pins, root):
    waiter_directory = root / "provider-waiters"
    waiter_directory.mkdir()
    pin_paths = []
    for index in range(200):
        path = waiter_directory / ("entry-%03d" % index)
        path.write_bytes(b"entry")
        pin_paths.append(path)
    control = root / "pueue-fixture.py"
    control.write_text(
        "#!" + sys.executable + "\n"
        "import json, sys\n"
        "if sys.argv[-2:] == ['status', '--json']:\n"
        " print(json.dumps({'tasks': {}}))\n"
        "elif sys.argv[-1] == 'shutdown':\n"
        " print('control-ok')\n"
        "else:\n"
        " raise SystemExit(2)\n"
    )
    control.chmod(0o700)
    config = root / "pueue.yml"
    config.write_text("fixture: true\n")
    client = control
    command = [str(client)]
    command_env = os.environ.copy()
    directory_flags = os.O_RDONLY
    directory_flags |= getattr(os, "O_DIRECTORY", 0)
    directory_flags |= getattr(os, "O_CLOEXEC", 0)
    parent_pin = os.open(waiter_directory, directory_flags)
    pins.append(parent_pin)
    for path in pin_paths:
        pins.append(os.open(path, os.O_RDONLY | getattr(os, "O_CLOEXEC", 0)))
    if len(pins) != 201:
        raise AssertionError("fixture did not hold the 200-entry plus shared-parent pins")

    fd_directory = "/dev/fd" if Path("/dev/fd").exists() else "/proc/self/fd"
    while fd_count(fd_directory) < 252:
        pins.append(os.open(pin_paths[0], os.O_RDONLY | getattr(os, "O_CLOEXEC", 0)))
    held_fd_count = fd_count(fd_directory)

    try:
        subprocess.run(
            command + ["--config", str(config), "status", "--json"],
            env=command_env, capture_output=True, text=True, timeout=5, check=False,
        )
    except OSError as error:
        pipe_probe_errno = error.errno
    else:
        raise AssertionError("pipe capture unexpectedly survived the controlled FD budget")
    if pipe_probe_errno != errno.EMFILE:
        raise AssertionError("pipe capture failed with unexpected errno %s" % pipe_probe_errno)
    return client, config, command_env, fd_directory, held_fd_count, pipe_probe_errno


def _join_parallel_captures(capture, marker_names, publish_release, startup_watchdog_seconds):
    with concurrent.futures.ThreadPoolExecutor(max_workers=16) as workers:
        futures = tuple(workers.submit(capture, index) for index in range(16))
        try:
            startup_deadline = time.monotonic() + startup_watchdog_seconds
            while True:
                markers_before_release = marker_names()
                completed_before_release = tuple(
                    index for index, future in enumerate(futures) if future.done()
                )
                if len(markers_before_release) == 16:
                    break
                if completed_before_release:
                    raise AssertionError(
                        "parallel capture completed before release: %s" %
                        (completed_before_release,)
                    )
                if time.monotonic() >= startup_deadline:
                    raise AssertionError(
                        "parallel capture startup watchdog expired with %d/16 markers" %
                        len(markers_before_release)
                    )
                time.sleep(0.005)
            pending_before_release = sum(not future.done() for future in futures)
            if pending_before_release != 16:
                raise AssertionError(
                    "parallel capture was not pending before release: %d/16" %
                    pending_before_release
                )
            publish_release()
            _done, not_done = concurrent.futures.wait(futures, timeout=70)
            if not_done:
                raise AssertionError(
                    "parallel capture join watchdog expired: %d incomplete" %
                    len(not_done)
                )
            results = []
            join_errors = []
            for future in futures:
                try:
                    results.append(future.result())
                except Exception as error:
                    join_errors.append(repr(error))
            if join_errors:
                raise AssertionError(
                    "parallel capture join errors=%s" % (join_errors,)
                )
        finally:
            publish_release()
        markers_after_release = marker_names()
        exit_codes = tuple(result[0] for result in results)
        outputs = tuple(result[1] for result in results)
        errors = tuple(result[2] for result in results)
        if exit_codes != (0,) * 16:
            raise AssertionError(
                "parallel capture exit codes=%s stderr=%s" % (exit_codes, errors)
            )
        if outputs != ("delegate-short-capture",) * 16:
            raise AssertionError("parallel capture outputs=%s" % (outputs,))
    return markers_before_release, pending_before_release, markers_after_release, exit_codes, outputs


if __name__ == "__main__":
    main()
