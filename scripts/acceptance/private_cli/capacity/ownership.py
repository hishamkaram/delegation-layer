"""Private CLI capacity ownership boundary."""

from pathlib import Path
import errno
import fcntl
import os
import stat
import time


def lease_candidate_specifications(specifications):
    """Wait for fixture entry publication; acquisition still proves liveness."""
    candidates = []
    for specification in specifications:
        try:
            Path(specification["path"]).lstat()
        except FileNotFoundError:
            continue
        candidates.append(specification)
    return candidates


def prove_lifetime_owner_cohort(specifications, lease_cohort):
    """Prove every gated task retains its exclusive lifetime-owner lease."""
    started_unix_ns = time.time_ns()
    started_monotonic_ns = time.monotonic_ns()
    evidence = []
    lease_cohort.assert_live()
    for index, specification in enumerate(sorted(specifications, key=lambda item: item["task_id"])):
        lock_path = specification["owner_lock"]
        descriptor = os.open(lock_path, os.O_RDWR | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK)
        try:
            info = os.fstat(descriptor)
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid():
                raise RuntimeError("capacity lifetime-owner lock is unsafe: " + specification["task_id"])
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                if error.errno not in (errno.EACCES, errno.EAGAIN):
                    raise
            else:
                fcntl.flock(descriptor, fcntl.LOCK_UN)
                raise RuntimeError("capacity task has no live lifetime owner: " + specification["task_id"])
        finally:
            os.close(descriptor)
        evidence.append(
            {
                "root_id": specification["root_id"],
                "task_id": specification["task_id"],
                "owner_sha256": specification["owner_sha256"],
                "registration_sha256": specification["registration_sha256"],
                "scope_kind": specification["scope_kind"],
                "assurance": specification["assurance"],
                "scope_plan_sha256": specification["scope_plan_sha256"],
                "scope_id_sha256": specification["scope_id_sha256"],
                "owner_lock": {"device": info.st_dev, "inode": info.st_ino},
            }
        )
        if (index + 1) % 16 == 0:
            lease_cohort.assert_live()
    lease_cohort.assert_live()
    ended_monotonic_ns = time.monotonic_ns()
    ended_unix_ns = time.time_ns()
    return {
        "started_unix_ns": started_unix_ns,
        "ended_unix_ns": ended_unix_ns,
        "duration_ns": max(0, ended_monotonic_ns - started_monotonic_ns),
        "lease_probe": "exclusive-flock-contended",
        "counted_task_count": len(evidence),
        "counted_tasks": evidence,
    }
