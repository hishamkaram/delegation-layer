"""Private CLI fingerprints boundary."""

from pathlib import Path
import hashlib
import os
import resource
import stat


REPOSITORY = Path(__file__).resolve().parents[3]

def file_fingerprint(path):
    """Digest permitted source/configuration inputs, never installed executable contents."""
    candidate = Path(path)
    try:
        resolved = candidate.resolve(strict=True)
        info = resolved.stat()
        if not resolved.is_file():
            raise OSError("not a regular file")
        return {
            "name": resolved.name,
            "bytes": info.st_size,
            "sha256": hashlib.sha256(resolved.read_bytes()).hexdigest(),
        }
    except OSError:
        return {"name": candidate.name, "status": "unavailable"}


def installed_executable_observation(path):
    """Observe the selected installation in place using only path metadata."""
    candidate = Path(path)
    try:
        resolved = candidate.resolve(strict=True)
        info = resolved.stat()
        if not stat.S_ISREG(info.st_mode) or not os.access(resolved, os.X_OK):
            raise OSError("not an executable regular file")
        return {"representation": "installed-paths", "path": str(resolved),
                "name": resolved.name, "bytes": info.st_size}
    except OSError:
        return {"name": candidate.name, "status": "unavailable"}


def descriptor_evidence():
    try:
        soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    except (OSError, ValueError):
        soft, hard = None, None

    def bounded_limit(value):
        if value is None:
            return "unknown"
        return "infinity" if value == resource.RLIM_INFINITY else value

    open_count = None
    count_source = None
    for directory in (Path("/proc/self/fd"), Path("/dev/fd")):
        try:
            open_count = len(os.listdir(directory))
            count_source = str(directory)
            break
        except OSError:
            continue
    result = {"soft_limit": bounded_limit(soft), "hard_limit": bounded_limit(hard), "open_count": open_count}
    if count_source is not None:
        result["count_source"] = count_source
    return result
