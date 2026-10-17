"""Codex acceptance request and prompt construction."""

from __future__ import annotations

import shlex
from pathlib import Path

from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.paths.operations import write_bytes
from scripts.delegate_acceptance.provider_drivers.codex.constants import TASK_BUDGET
from scripts.delegate_acceptance.provider_drivers.codex.runtime import no_prompt_argv


def write_fresh_brief(path: Path, probe: Path, nonce: bytes) -> Path:
    """Write the constrained command request without embedding the nonce."""
    quoted_probe = shlex.quote(str(probe))
    text = (
        "Use command_execution only, in the current workspace. Do not use file editing, agents, networking, "
        "or an approval host. Run exactly one command_execution with `/bin/sh " + quoted_probe + "`. Do not run "
        "cat, printf, another shell command, or any other tool. Return the probe's actual output and exit "
        "status, followed by a concise summary. Do not claim read or write results from prose."
    )
    write_bytes(path, text.encode())
    require(nonce.decode() not in path.read_text(), "fresh brief contains the nonce")
    return path


def write_resume_brief(path: Path, nonce: bytes) -> Path:
    """Write the continuation request without exposing the nonce in its prompt."""
    text = ("Continue the exact previous Codex conversation. Do not use any tool and do not read any file. "
            "Return only the random nonce you reported in the previous turn, with no surrounding prose.")
    write_bytes(path, text.encode())
    require(nonce.decode() not in path.read_text(), "resume brief contains the nonce")
    return path


def dispatch_arguments(delegate: Path, state: Path, pueue_config: Path, runner: Path,
                       task: str, brief: Path, workspace: Path, provider: str,
                       mode: str, predecessor: str | None = None) -> list[object]:
    """Build one canonical Codex dispatch argv and check prompt transport."""
    args: list[object] = [delegate, "--root", state, "--pueue-config", pueue_config,
                          "--runner", runner, "dispatch", "--provider", provider,
                          "--brief", brief, "--cwd", workspace, "--id", task,
                          "--permission", mode, "--budget", TASK_BUDGET, "--json"]
    if predecessor is not None:
        args.extend(["--resume-task", predecessor])
    no_prompt_argv(args, [brief])
    return args
