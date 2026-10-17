"""Pure inspection result and verdict rules over supplied validated facts."""

from __future__ import annotations

from datetime import datetime
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp, _timestamp_before
from scripts.delegate_acceptance.evidence.validation import _exact_keys, require


def _validate_inspection_verdict(
        allow_failure: bool,
        authority: dict[str, object],
        completion: dict[str, object] | None,
        deadline: tuple[datetime, int],
        historical_aborted: bool,
        managed_lifetime: dict[str, object] | None,
        managed_worker_succeeded: bool,
        observation: dict[str, object] | None,
        ordinary_matches: bool,
        result: dict[str, object] | None,
        standalone: bool,
        task: str,
) -> None:
    if historical_aborted:
        # Admission closure proves that no ordinary task was created.
        # The helper can independently have succeeded, failed, or died
        # before publishing a result. Its own closure remains mandatory.
        require(authority["mode"] != "guardian-unstarted",
                f"managed historical inspection closure is absent for {task}")
    elif allow_failure:
        if result is None:
            require(not ordinary_matches and observation is None and
                    (managed_lifetime is None or not managed_worker_succeeded),
                    f"failed inspection has an ordinary task or observation for {task}")
        elif result.get("reason") == "eligible":
            require((standalone or ordinary_matches) and completion is not None and
                    completion.get("native_exit") == "successful-exit" and
                    ((managed_lifetime is not None and managed_worker_succeeded) or
                     observation is not None),
                    f"eligible inspection lacks independent success proof for {task}")
        else:
            require(not ordinary_matches and observation is None and
                    completion is not None and completion.get("native_exit") == "unavailable" and
                    (managed_lifetime is None or not managed_worker_succeeded),
                    f"failed inspection has conflicting success proof for {task}")
    else:
        require(result is not None and result.get("reason") == "eligible" and
                (standalone or ordinary_matches) and completion is not None and
                completion.get("native_exit") == "successful-exit" and
                ((managed_lifetime is not None and managed_worker_succeeded) or
                 observation is not None),
                f"successful inspection proof is incomplete for {task}")
        if managed_lifetime is not None:
            termination = managed_lifetime["termination"]
            require(isinstance(termination, dict) and
                    termination.get("cause") == "completed" and
                    _timestamp_before(_canonical_timestamp(
                        termination.get("observed_at"),
                        f"inspection lifetime termination observed_at for {task}"), deadline),
                    f"successful inspection lifetime proof expired for {task}")


def _validate_inspection_result(
        completion: dict[str, object] | None,
        observation: dict[str, object] | None,
        request_digest: str,
        result: dict[str, object] | None,
        task: str,
) -> None:
    if result is None:
        require(completion is None and observation is None,
                f"inspection completion or observation has no result for {task}")
    else:
        _exact_keys(result, {"schema_version", "request_sha256", "reason", "facts"},
                    label=f"inspection result for {task}")
        require(type(result.get("schema_version")) is int and result["schema_version"] == 1 and
                result.get("request_sha256") == request_digest,
                f"inspection result binding mismatch for {task}")
        require(result.get("reason") in {"eligible", "unavailable", "deadline-expired"},
                f"inspection result reason is invalid for {task}")
        require(isinstance(result.get("facts"), dict),
                f"inspection result facts are invalid for {task}")
        if result.get("reason") != "eligible":
            require(result["facts"] == {},
                    f"noneligible inspection result contains facts for {task}")
