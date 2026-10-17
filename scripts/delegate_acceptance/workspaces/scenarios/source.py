"""Fixture source, Git evidence, and provider fixture setup."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil

import scripts.delegate_acceptance.private_cli.fixtures as private_cli_fixtures
from scripts.delegate_acceptance.processes.operations import Process
from scripts.delegate_acceptance.directories.policy import require
from .context import WorkspaceContext
from .model import (
    AcceptanceBlocked, FIXTURE_SESSION, MAX_RESPONSE_BYTES,
)

REPOSITORY = Path(__file__).resolve().parents[4]
ENTRYPOINT_PATH = REPOSITORY / "scripts" / "acceptance_workspaces.py"


def _require_executable(path: Path, label: str) -> Path:
    try:
        resolved = path.resolve(strict=True)
    except OSError as error:
        raise AcceptanceBlocked(label + "-unavailable") from error
    if not resolved.is_file() or not os.access(resolved, os.X_OK):
        raise AcceptanceBlocked(label + "-not-executable")
    return resolved



def _git_executable() -> Path:
    found = shutil.which("git")
    if not found:
        raise AcceptanceBlocked("git-unavailable")
    return _require_executable(Path(found), "git")


def _git_process(
    context: WorkspaceContext,
    arguments: list[str],
    cwd: Path,
    *,
    label: str,
    expected: tuple[int, ...] = (0,),
    timeout: float = 30,
) -> Process:
    return context.processes.run(
        "git-" + label,
        [_git_executable(), *arguments],
        cwd,
        expected=expected,
        timeout=timeout,
    )


def _git(
    context: WorkspaceContext,
    arguments: list[str],
    cwd: Path,
    *,
    label: str,
    expected: tuple[int, ...] = (0,),
    timeout: float = 30,
) -> tuple[int, bytes]:
    process = _git_process(context, arguments, cwd, label=label, expected=expected, timeout=timeout)
    return process.result["exit_code"], process.output(MAX_RESPONSE_BYTES)


def _git_ok(context: WorkspaceContext, arguments: list[str], cwd: Path, *, label: str) -> bytes:
    code, output = _git(context, arguments, cwd, label=label)
    require(code == 0, "git command returned unexpected exit: " + label)
    return output


def _prepare_source(context: WorkspaceContext) -> None:
    source = context.source
    (source / "tracked.txt").write_text("managed workspace source\n", encoding="utf-8")
    (source / "README.md").write_text("managed workspace acceptance source\n", encoding="utf-8")
    _git_ok(context, ["init", "--quiet", "--initial-branch=main"], source, label="init")
    _git_ok(context, ["config", "user.name", "Acceptance Fixture"], source, label="config-name")
    _git_ok(context, ["config", "user.email", "fixture@example.invalid"], source, label="config-email")
    _git_ok(context, ["add", "--", "README.md", "tracked.txt"], source, label="add")
    _git_ok(context, ["commit", "--quiet", "-m", "fixture base"], source, label="commit")
    head = _git_ok(context, ["rev-parse", "--verify", "HEAD^{commit}"], source, label="baseline-head")
    tree = _git_ok(context, ["rev-parse", "--verify", "HEAD^{tree}"], source, label="baseline-tree")
    status = _git_ok(context, ["status", "--porcelain=v1", "--untracked-files=all"], source, label="baseline-status")
    require(not status.strip(), "fixture source is not clean after baseline commit")
    context.source_baseline = {
        "head": head.decode("ascii").strip(),
        "tree": tree.decode("ascii").strip(),
        "status_sha256": hashlib.sha256(status).hexdigest(),
    }


def _assert_source_untouched(context: WorkspaceContext) -> dict[str, object]:
    head = _git_ok(context, ["rev-parse", "--verify", "HEAD^{commit}"], context.source, label="source-final-head")
    tree = _git_ok(context, ["rev-parse", "--verify", "HEAD^{tree}"], context.source, label="source-final-tree")
    status = _git_ok(context, ["status", "--porcelain=v1", "--untracked-files=all"], context.source, label="source-final-status")
    observed = {
        "head": head.decode("ascii").strip(),
        "tree": tree.decode("ascii").strip(),
        "status_sha256": hashlib.sha256(status).hexdigest(),
        "status_empty": not status.strip(),
    }
    require(observed["head"] == context.source_baseline["head"], "source HEAD changed")
    require(observed["tree"] == context.source_baseline["tree"], "source tree changed")
    require(observed["status_empty"] is True, "source worktree is dirty")
    return observed


def _git_input_paths(context: WorkspaceContext) -> list[str]:
    output = _git_ok(
        context,
        ["-C", str(REPOSITORY), "ls-files", "--cached", "--others",
         "--exclude-standard", "-z"],
        REPOSITORY,
        label="source-manifest-paths",
    )
    names = output.decode("utf-8").split("\x00")
    paths = [name for name in names if name]
    require(len(paths) <= 8192, "source manifest exceeds entry bound")
    return paths


def build_source_manifest(context: WorkspaceContext) -> dict[str, object]:
    files: list[dict[str, object]] = []
    for name in _git_input_paths(context):
        path = REPOSITORY / name
        require(path.is_file() and not path.is_symlink(), "source manifest input is not a regular file: " + name)
        data = path.read_bytes()
        files.append({"path": name, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()})
    manifest = {
        "status": "complete",
        "root": REPOSITORY.name,
        "tracked_and_new_input_count": len(files),
        "files": files,
    }
    manifest_bytes = json.dumps(manifest, sort_keys=True, separators=(",", ":")).encode()
    manifest["manifest_sha256"] = hashlib.sha256(manifest_bytes).hexdigest()
    return manifest


def _install_workspace_fixture(
    provider_bin: Path,
    gate: Path,
    waiters: Path,
    nonce: str,
    session_marker: Path,
) -> None:
    private_cli_fixtures.install_pi_fixture(provider_bin, gate, waiters, nonce)
    fixture = provider_bin / "pi"
    original = fixture.read_text(encoding="utf-8")
    marker = "prompt=$(/bin/cat)\n"
    require(original.count(marker) == 1, "private fixture shape changed")
    session_line = '{"type":"session","id":"' + FIXTURE_SESSION + '"}\n'
    require(original.count(session_line) == 1, "private fixture session event shape changed")
    original = original.replace(session_line, "", 1)
    wait_program = private_cli_fixtures.PI_WAIT_PROGRAM
    wait_session_line = '{"type":"session","id":"' + FIXTURE_SESSION + '"}\\n'
    require(wait_program.count(wait_session_line) == 1,
            "private fixture wait session event shape changed")
    original_wait_program = shlex.quote(wait_program)
    fixture_wait_program = shlex.quote(wait_program.replace(wait_session_line, "", 1))
    require(original.count(original_wait_program) == 1,
            "private fixture wait program shape changed")
    original = original.replace(original_wait_program, fixture_wait_program, 1)
    transformation = r'''prompt=$(/bin/cat)
session_id=__FIXTURE_SESSION_JSON__
session_marker=__FIXTURE_SESSION_MARKER_JSON__
requested_session=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --session)
      if [ "$#" -lt 2 ]; then
        printf '%s\n' 'fixture continuation omitted --session value' >&2
        exit 91
      fi
      requested_session=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
if [ -n "$requested_session" ] && [ "$requested_session" != "$session_id" ]; then
  printf '%s\n' 'fixture continuation session mismatch' >&2
  exit 92
fi
resumed=false
if [ -n "$requested_session" ]; then
  resumed=true
fi
printf '{"schema_version":1,"session":"00000000-0000-4000-8000-000000000001","resumed":%s}\n' "$resumed" > "$session_marker"
printf '%s\n' '{"type":"session","id":"00000000-0000-4000-8000-000000000001"}'
case "$prompt" in
  *DELEGATE_WORKSPACE_TIMEOUT_RESUME*)
    printf '%s\n' 'fixture workspace timeout continuation' > managed-workspace-timeout-resumed.txt
    git add -N -- managed-workspace-timeout-resumed.txt
    ;;
  *DELEGATE_WORKSPACE_REUSE_FIXTURE*)
    printf '%s\n' 'fixture workspace reused edit' > managed-workspace-reused.txt
    git add -N -- managed-workspace-reused.txt
    ;;
  *DELEGATE_WORKSPACE_FIXTURE*|*DELEGATE_WORKSPACE_TIMEOUT*)
    printf '%s\n' 'fixture workspace edit' > managed-workspace-fixture.txt
    git add -N -- managed-workspace-fixture.txt
    ;;
esac
'''
    transformation = transformation.replace(
        "__FIXTURE_SESSION_JSON__", shlex.quote(FIXTURE_SESSION),
    ).replace(
        "__FIXTURE_SESSION_MARKER_JSON__", shlex.quote(str(session_marker)),
    )
    fixture.write_text(original.replace(marker, transformation, 1), encoding="utf-8")
    fixture.chmod(0o700)


def _write_briefs(context: WorkspaceContext) -> None:
    context.brief_success.write_text(
        "DELEGATE_WORKSPACE_FIXTURE\nReply with the fixture acceptance sentence.\n",
        encoding="utf-8",
    )
    context.brief_success_reuse.write_text(
        "DELEGATE_WORKSPACE_REUSE_FIXTURE\nReply with the fixture reuse sentence.\n",
        encoding="utf-8",
    )
    context.brief_timeout.write_text(
        "DELEGATE_WORKSPACE_TIMEOUT\nDELEGATE_PRIVATE_UPGRADE_WAIT\n",
        encoding="utf-8",
    )
    context.brief_timeout_resume.write_text(
        "DELEGATE_WORKSPACE_TIMEOUT_RESUME\nReply with the fixture continuation sentence.\n",
        encoding="utf-8",
    )
    context.brief_capacity.write_text(
        "DELEGATE_WORKSPACE_FIXTURE\nDELEGATE_PRIVATE_UPGRADE_WAIT\n",
        encoding="utf-8",
    )
    for brief in (
        context.brief_success,
        context.brief_success_reuse,
        context.brief_timeout,
        context.brief_timeout_resume,
        context.brief_capacity,
    ):
        brief.chmod(0o600)
