"""Release command receipts over the canonical bounded child-operation owner."""

from scripts.delegate_acceptance.processes.contracts import BoundedCommandError
from scripts.delegate_acceptance.processes.operations import run_bounded_command
from .contract import MAX_OUTPUT, VerificationError


def command(receipt: dict, name: str, argv: list[str], env: dict[str, str], *, capture=False, timeout=30) -> bytes:
    record = {"name": name, "argv": argv, "exit_code": None}
    commands = receipt.get("commands")
    if not isinstance(commands, list):
        raise VerificationError("receipt_commands_invalid", name)
    commands.append(record)
    try:
        result = run_bounded_command(
            argv, env, capture=capture, timeout=timeout, max_output=MAX_OUTPUT,
        )
    except BoundedCommandError as error:
        if capture:
            record["exit_code"] = error.exit_code
        raise VerificationError(error.reason.value, name) from None
    record["exit_code"] = result.exit_code
    if result.exit_code:
        raise VerificationError("command_failed", name)
    return result.output
