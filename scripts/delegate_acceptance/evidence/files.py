"""Bounded evidence files and durable create-once JSON publication."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from scripts.delegate_acceptance.directories.operations import (
    _DIRECTORY_OPEN_FLAGS,
    _close_parent_descriptor,
    _rename_noreplace,
)
from scripts.delegate_acceptance.directories.policy import require
from pathlib import Path


def digest(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as stream:
        while block := stream.read(1024 * 1024):
            value.update(block)
    return value.hexdigest()


def write_json(path, value, replace=False):
    path = Path(path)
    data = (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()
    descriptor, stage_name = tempfile.mkstemp(
        prefix="." + path.name + ".", suffix=".staging", dir=path.parent,
    )
    stage = Path(stage_name)
    parent = -1
    try:
        stream = os.fdopen(descriptor, "wb")
        descriptor = -1
        with stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(stage, 0o600)
        parent = os.open(path.parent, _DIRECTORY_OPEN_FLAGS)
        if replace:
            os.replace(stage, path)
        else:
            _rename_noreplace(parent, stage.name, parent, path.name)
        os.fsync(parent)
    finally:
        _close_parent_descriptor(descriptor)
        _close_parent_descriptor(parent)
        try:
            stage.unlink()
        except FileNotFoundError:
            pass


def read_json(path, bound=1024 * 1024):
    with Path(path).open("rb") as stream:
        data = stream.read(bound + 1)
    require(len(data) <= bound, "JSON evidence exceeds bound: " + str(path))
    def pairs(entries):
        obj = {}
        for key, value in entries:
            require(key not in obj, "duplicate evidence key: " + key)
            obj[key] = value
        return obj
    return json.loads(data, object_pairs_hook=pairs)
