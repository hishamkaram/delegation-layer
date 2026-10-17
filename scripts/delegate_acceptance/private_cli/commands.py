"""Private CLI commands boundary."""

from scripts.delegate_acceptance.private_cli.capture import output_tail
from scripts.delegate_acceptance.private_cli.capture import run_captured_command
from scripts.delegate_acceptance.private_cli.environment import phase_environment
import hashlib
import json
import subprocess


def record_invocation_error(receipt, args, error, captured):
    if receipt is None:
        return
    stdout = captured.stdout if captured is not None else getattr(error, "output", None) or b""
    stderr = captured.stderr if captured is not None else getattr(error, "stderr", None) or b""
    evidence = {
        "arguments": args,
        "invocation_error": str(error),
        "failure_class": "invocation_error",
        "exit_code": captured.returncode if captured is not None else None,
        "stdout_bytes": len(stdout),
        "stdout_sha256": hashlib.sha256(stdout).hexdigest(),
        "stdout_tail": output_tail(stdout),
        "stderr_bytes": len(stderr),
        "stderr_sha256": hashlib.sha256(stderr).hexdigest(),
        "stderr_tail": output_tail(stderr),
    }
    retained_paths = getattr(error, "retained_capture_paths", ())
    if retained_paths:
        evidence["capture_failure"] = {
            "retained_paths": list(retained_paths),
            "cause": (
                None
                if error.__cause__ is None
                else {"type": type(error.__cause__).__name__, "message": str(error.__cause__)}
            ),
        }
    receipt.write_text(json.dumps(evidence, indent=2) + "\n")


def run_json(
    delegate, env, args, expected=(0,), timeout=45, receipt=None, phase=None, *, capture_root, capture_response=False
):
    captured = None
    try:
        command_env = phase_environment(env, phase) if phase is not None else env
        captured = run_captured_command([str(delegate), *args], command_env, timeout, capture_root=capture_root)
        result = subprocess.CompletedProcess(
            captured.args, captured.returncode, captured.stdout.decode("utf-8"), captured.stderr.decode("utf-8")
        )
    except (OSError, subprocess.TimeoutExpired, UnicodeDecodeError) as error:
        record_invocation_error(receipt, args, error, captured)
        raise
    if receipt is not None:
        evidence = {
            "arguments": args,
            "exit_code": result.returncode,
            "stdout_bytes": len(captured.stdout),
            "stdout_sha256": hashlib.sha256(captured.stdout).hexdigest(),
            "stderr_bytes": len(captured.stderr),
            "stderr_sha256": hashlib.sha256(captured.stderr).hexdigest(),
        }
        if result.returncode != 0:
            evidence.update(
                {
                    "failure_class": "nonzero_exit",
                    "stdout_tail": output_tail(captured.stdout),
                    "stderr_tail": output_tail(captured.stderr),
                }
            )
        receipt.write_text(json.dumps(evidence, indent=2) + "\n")
    if result.returncode not in expected:
        raise RuntimeError(
            "delegate command failed: %r exit=%d stdout=%r stderr=%r"
            % (args, result.returncode, result.stdout[-2000:], result.stderr[-2000:])
        )
    try:
        response = json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise RuntimeError("delegate returned invalid JSON for %r: %s" % (args, error)) from error
    if receipt is not None and capture_response:
        evidence["response"] = response
        receipt.write_text(json.dumps(evidence, indent=2) + "\n")
    return response
