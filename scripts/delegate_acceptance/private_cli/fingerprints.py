"""Private CLI fingerprints boundary."""

from pathlib import Path
import hashlib
import os
import resource


REPOSITORY = Path(__file__).resolve().parents[3]

def file_fingerprint(path):
    """Return a compact, path-safe identity for one source or executable."""
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
