"""Supported queued-runner placement: execute the selected installation in place."""

from __future__ import annotations

from typing import Literal


RunnerPlacement = Literal["bundle"]
BUNDLE_RUNNER: RunnerPlacement = "bundle"
RUNNER_PLACEMENTS = frozenset({BUNDLE_RUNNER})
