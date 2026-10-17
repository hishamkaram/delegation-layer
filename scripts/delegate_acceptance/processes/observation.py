"""Finite observation deadlines and inherited acceptance environment."""

from __future__ import annotations

import math
import os
import time
from scripts.delegate_acceptance.directories.policy import require


def failure_cleanup_deadline(timeout):
    """Return one finite monotonic deadline for failure-path observation."""
    require(type(timeout) in (int, float) and math.isfinite(timeout) and timeout > 0,
            "failure cleanup timeout must be finite and positive")
    return time.monotonic() + float(timeout)


def wait_for_failure_cleanup(deadline, delay):
    """Sleep within a cleanup deadline; return false when its budget is spent."""
    require(type(deadline) in (int, float) and math.isfinite(deadline),
            "failure cleanup deadline is invalid")
    require(type(delay) in (int, float) and math.isfinite(delay) and delay >= 0,
            "failure cleanup delay is invalid")
    remaining = float(deadline) - time.monotonic()
    if remaining <= 0:
        return False
    time.sleep(min(float(delay), remaining))
    return True


def wait_until(predicate, timeout=15, description="condition", *, timeout_error=None):
    deadline = time.monotonic() + timeout
    while True:
        value = predicate()
        if value:
            return value
        within_deadline = time.monotonic() < deadline
        if not within_deadline and timeout_error is not None:
            raise timeout_error
        require(within_deadline, "observation expired: " + description)
        time.sleep(0.05)


def inherited_environment():
    # Each included coordinate is inherited without relocation or PATH shadowing.
    keys = {"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL",
            "LC_CTYPE", "TZ", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
            "XDG_RUNTIME_DIR", "__CF_USER_TEXT_ENCODING"}
    return {key: value for key, value in os.environ.items() if key in keys}
