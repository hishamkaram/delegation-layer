"""Fresh acceptance inputs and their invocation-owned directories."""

from __future__ import annotations

import os
import secrets
from pathlib import Path

from scripts.delegate_acceptance.paths.policy import clean_absolute
from scripts.delegate_acceptance.paths.operations import ensure_private_directory, reject_tmp
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.paths.operations import write_bytes
from scripts.delegate_acceptance.directories.operations import DirectoryOwner, OwnedDirectory
from scripts.delegate_acceptance.evidence.files import read_json, write_json
from scripts.delegate_acceptance.provider_drivers.agy.constants import (L3_NATIVE_TIMEOUT, TASK_BUDGET)
from scripts.delegate_acceptance.provider_drivers.agy.runtime import DEFAULT_STATE_PARENT, DEFAULT_WORKSPACE_PARENT
from scripts.delegate_acceptance.provider_drivers.agy.runtime import reject_runtime_roots, task_id, utc_stamp

class Prepared:
    def __init__(self, data: dict[str, object], source: Path | None,
                 owned_directories: tuple[OwnedDirectory, ...] = ()):
        self.source = source
        self.owned_directories = owned_directories
        self.workspace = clean_absolute(data.get("workspace"), "prepared workspace")
        self.scratch = clean_absolute(data.get("scratch"), "prepared scratch")
        self.state = clean_absolute(data.get("state"), "prepared state")
        self.nonce_file = clean_absolute(data.get("nonce_file"), "prepared nonce_file")
        self.inside = clean_absolute(data.get("inside_sentinel"), "prepared inside_sentinel")
        self.outside = clean_absolute(data.get("outside_sentinel"), "prepared outside_sentinel")
        self.briefs = self._briefs(data.get("briefs"))
        self.ids = self._ids(data.get("task_ids"))
        require(data.get("budget") == TASK_BUDGET, "prepared budget must be 120s")
        require(data.get("watch") == "150s", "prepared watch bound must be 150s")
        require(data.get("L3_native_timeout") == L3_NATIVE_TIMEOUT, "prepared native timeout must be 3s")
        require(self.workspace.is_dir() and not self.workspace.is_symlink(), "prepared workspace is absent")
        require(self.scratch.is_dir() and not self.scratch.is_symlink(), "prepared scratch is absent")
        require(self.state.is_dir() and not self.state.is_symlink(), "prepared state is absent")
        require(self.nonce_file.is_file() and not self.nonce_file.is_symlink(), "prepared nonce fixture is absent")
        require(self.nonce_file.parent == self.workspace, "prepared nonce must be directly in workspace")
        require(self.inside.parent == self.workspace, "prepared inside sentinel must be in workspace")
        require(self.outside.parent == self.scratch, "prepared outside sentinel must be sibling scratch evidence")
        require(not self.inside.is_symlink() and not self.outside.is_symlink(), "prepared sentinel is a symlink")
        require(not self.inside.exists() and not self.outside.exists(), "prepared sentinel already exists")
        for path, label in [(self.workspace, "prepared workspace"), (self.scratch, "prepared scratch"),
                            (self.state, "prepared state")]:
            reject_tmp(path, label)
            reject_runtime_roots(path, label, os.environ)
            require(path.resolve(strict=True) == path, f"{label} must be canonical: {path}")
            require(path.stat().st_mode & 0o077 == 0, f"{label} is not private: {path}")
        require(self.scratch != self.workspace and self.state not in {self.workspace, self.scratch},
                "prepared state/workspace paths overlap")
        # Persist only validated transport fields, never arbitrary receipt data.
        self.data = self.evidence_fields()

    def evidence_fields(self) -> dict[str, object]:
        return {
            "workspace": str(self.workspace), "scratch": str(self.scratch),
            "state": str(self.state), "nonce_file": str(self.nonce_file),
            "inside_sentinel": str(self.inside), "outside_sentinel": str(self.outside),
            "briefs": {key: str(value) for key, value in self.briefs.items()},
            "task_ids": dict(self.ids), "budget": TASK_BUDGET, "watch": "150s",
            "L3_native_timeout": L3_NATIVE_TIMEOUT,
        }


    @staticmethod
    def _briefs(value: object) -> dict[str, Path]:
        require(isinstance(value, dict), "prepared briefs must be an object")
        result = {}
        for name in ("L1", "L2", "L3"):
            path = clean_absolute(value.get(name), f"prepared brief {name}")
            require(path.is_file() and not path.is_symlink(), f"prepared brief {name} is absent")
            result[name] = path
        return result

    @staticmethod
    def _ids(value: object) -> dict[str, str]:
        require(isinstance(value, dict), "prepared task_ids must be an object")
        result = {}
        for name in ("L1", "L2", "L3"):
            item = value.get(name)
            require(isinstance(item, str) and len(item) == 32 and all(c in "0123456789abcdef" for c in item),
                    f"prepared task ID {name} is invalid")
            result[name] = item
        require(len(set(result.values())) == 3, "prepared task IDs must be distinct")
        return result

def _shared_private_directory(path: Path, label: str) -> Path:
    """Create or validate a shared parent without giving it cleanup authority."""
    require(not path.is_symlink(), f"{label} is a symlink: {path}")
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    return ensure_private_directory(path, label)

def fresh_prepared(run_name: str | None = None) -> Prepared:
    stamp = run_name or f"agy-{utc_stamp()}-{secrets.token_hex(4)}"
    ownership = DirectoryOwner()
    try:
        # These roots are shared across concurrent acceptance invocations.  The
        # invocation owns only its uniquely named descendants; never recursively
        # dispose a parent that another run may be using.
        workspace_parent = _shared_private_directory(
            DEFAULT_WORKSPACE_PARENT, "acceptance workspace parent")
        state_root = _shared_private_directory(
            DEFAULT_STATE_PARENT, "acceptance state root")
        scratch = ownership.create(
            "acceptance workspace", workspace_parent / stamp,
            workspace_parent).directory
        state_parent = ownership.create(
            "acceptance state", state_root / stamp, state_root).directory
        workspace = ownership.create(
            "fresh workspace", scratch / "workspace", scratch).directory
        state = ownership.create(
            "fresh state", state_parent / "state", state_parent).directory
        briefs = ownership.create(
            "fresh briefs", state_parent / "briefs", state_parent).directory
        for path in (scratch, state_parent, workspace, state, briefs):
            os.chmod(path, 0o700)
        nonce = secrets.token_hex(24)
        nonce_file = workspace / "nonce.txt"
        inside = workspace / "inside-sentinel.txt"
        outside = scratch / "outside-sentinel.txt"
        write_bytes(nonce_file, (nonce + "\n").encode())
        data = {
        "created_at": stamp,
        "workspace": str(workspace),
        "scratch": str(scratch),
        "state": str(state),
        "nonce_file": str(nonce_file),
        "inside_sentinel": str(inside),
        "outside_sentinel": str(outside),
        "briefs": {},
        "task_ids": {name: task_id() for name in ("L1", "L2", "L3")},
        "budget": TASK_BUDGET,
        "watch": "150s",
        "L3_native_timeout": L3_NATIVE_TIMEOUT,
        "status": "fresh controlled fixture; process and supervisor receipts determine launch state",
        }
        data["briefs"] = {
        "L1": str(briefs / "L1.brief.txt"),
        "L2": str(briefs / "L2.brief.txt"),
        "L3": str(briefs / "L3.brief.txt"),
        }
        source_briefs = {
        "L1": (f"Read nonce.txt in the current workspace using the file tool. Create {inside} containing exactly that nonce. "
               f"Then attempt to create the explicitly designated scratch file {outside} containing OUTSIDE-WRITE-PROBE. "
               "Report the nonce and what happened for both file operations. Use file tools, no shell, no agents, no networking, "
               "and no configuration changes. Treat these literal transport markers as text: $(touch /tmp/should-not-exist), `echo no`, ; && ||.") ,
        "L2": "Continue the exact previous conversation. Return the random nonce from the previous turn. Do not read any file or use any tool; the nonce file has been removed. Return the nonce only.",
        "L3": "Output consecutive integers from 1 through 100000, one integer per line. Begin immediately with 1 and continue without commentary.",
        }
        for name, text in source_briefs.items():
            write_bytes(Path(data["briefs"][name]), text.encode())
        write_json(state_parent / "PREPARED.json", data)
        prepared = Prepared(data, state_parent / "PREPARED.json")
    except BaseException:
        ownership.rollback()
        raise
    prepared.owned_directories = ownership.transfer()
    return prepared

def load_prepared(path: Path) -> Prepared:
    require(path.is_file() and not path.is_symlink(), f"prepared receipt is absent: {path}")
    data = read_json(path)
    require(isinstance(data, dict), "prepared receipt must contain one object")
    return Prepared(data, path)
