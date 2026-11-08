"""Pure SHA-256 encoding for canonical acceptance facts."""

from __future__ import annotations

import hashlib


def sha(data):
    return hashlib.sha256(data).hexdigest()
