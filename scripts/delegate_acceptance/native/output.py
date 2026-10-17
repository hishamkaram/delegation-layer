"""Filesystem edge for selecting an owned native acceptance output."""

from __future__ import annotations

from datetime import datetime, timezone
from pathlib import Path
import secrets
from typing import Callable

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.paths.operations import reject_tmp
from scripts.delegate_acceptance.paths.policy import clean_absolute


PreparedCaseValidator = Callable[[Path, str, str, str], Path]


def choose_output(
        provider: str, explicit: str | None, permission: str | None, scenario: str | None,
        validate_prepared_case: PreparedCaseValidator,
        evidence_error: type[Exception]) -> Path:
    if explicit:
        path = clean_absolute(explicit, "live acceptance output")
    else:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        path = (Path.home() / ".local" / "state" / "delegation-layer-evidence" /
                f"{provider.replace(':', '-')}-{stamp}")
    reject_tmp(path, "live acceptance output")
    if path.exists() or path.is_symlink():
        if explicit and permission is not None and scenario is not None:
            try:
                return validate_prepared_case(path, provider, permission, scenario)
            except evidence_error as error:
                raise AcceptanceFailure(
                    "prepared live acceptance output is invalid: " + str(error)) from error
        raise AcceptanceFailure(f"live acceptance output already exists: {path}")
    path.mkdir(mode=0o700, parents=True, exist_ok=False)
    return path.resolve()
