"""Narrow curl stand-in that serves exact local candidate files and records every request."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import os
from .candidate_install_contract import RequestRecord, parse_request_log
from .contract import MAX_OUTPUT, VerificationError
from .filesystem import _bounded_file_bytes
from .published_files import create_private_file


MAP_ENV = "DELEGATION_CANDIDATE_SHIM_MAP"


LOG_ENV = "DELEGATION_CANDIDATE_SHIM_LOG"


# The installer runs exactly: curl -fsSL <timeouts and retries> -o OUT URL. Any other
# option (for example the latest-release lookup's -w) or URL is refused and recorded.
_SCRIPT = """#!/bin/sh
set -u
map="${DELEGATION_CANDIDATE_SHIM_MAP:?}"
log="${DELEGATION_CANDIDATE_SHIM_LOG:?}"
deny() {
  printf 'denied\\t%s\\n' "$1" >> "$log"
  printf 'curl: candidate shim refused %s\\n' "$1" >&2
  exit 22
}
out=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -fsSL) shift ;;
    --connect-timeout|--max-time|--retry|--retry-delay|--retry-max-time)
      [ "$#" -ge 2 ] || deny "option:$1"
      shift 2 ;;
    -o)
      [ "$#" -ge 2 ] || deny "option:$1"
      out="$2"
      shift 2 ;;
    -*) deny "option:$1" ;;
    *)
      [ -z "$url" ] || deny "extra:$1"
      url="$1"
      shift ;;
  esac
done
[ -n "$url" ] && [ -n "$out" ] || deny "incomplete"
tab="$(printf '\\t')"
while IFS="$tab" read -r known path; do
  if [ "$known" = "$url" ]; then
    cat "$path" > "$out" || deny "unreadable:$url"
    printf 'served\\t%s\\n' "$url" >> "$log"
    exit 0
  fi
done < "$map"
deny "$url"
"""


@dataclass(frozen=True)
class CurlShim:
    """Private PATH directory plus the answer map and request log it owns."""

    directory: Path
    map_path: Path
    log_path: Path

    def environment(self) -> dict[str, str]:
        return {MAP_ENV: str(self.map_path), LOG_ENV: str(self.log_path)}

    def requests(self) -> tuple[RequestRecord, ...]:
        return parse_request_log(_bounded_file_bytes(self.log_path, MAX_OUTPUT, "candidate_request_log_invalid"))


def _write_new(path: Path, data: bytes, mode: int) -> None:
    try:
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), mode)
    except OSError:
        raise VerificationError("candidate_shim_invalid", "prepare-shim") from None
    with os.fdopen(descriptor, "wb") as output:
        os.fchmod(output.fileno(), mode)
        output.write(data)


def prepare_shim(base: Path, answers: dict[str, Path]) -> CurlShim:
    """Create base/shim (below an owned workspace) answering only the given URLs."""
    directory = base / "shim"
    directory.mkdir(mode=0o700)
    shim = CurlShim(directory, base / "shim-answers.tsv", base / "shim-requests.log")
    _write_new(directory / "curl", _SCRIPT.encode("utf-8"), 0o700)
    lines = "".join(f"{url}\t{path}\n" for url, path in answers.items())
    _write_new(shim.map_path, lines.encode("utf-8"), 0o600)
    create_private_file(shim.log_path)
    return shim
