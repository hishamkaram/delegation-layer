"""Private CLI lease policy boundary."""

from pathlib import Path


def lease_fifo_path(waiter_directory, nonce, pid):
    if len(nonce) != 32 or any(char not in "0123456789abcdef" for char in nonce):
        raise RuntimeError("capacity lease nonce is invalid")
    if not isinstance(pid, int) or pid <= 0:
        raise RuntimeError("capacity lease PID is invalid")
    return Path(waiter_directory) / (nonce + "-" + str(pid) + ".fifo")
