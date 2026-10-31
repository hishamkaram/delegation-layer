"""Private CLI wrapper policy boundary."""

from scripts.delegate_acceptance.private_cli.configuration import PROTECTED_CALLER_PHASES
from scripts.delegate_acceptance.private_cli.configuration import STARTUP_CONTROL_COMMANDS
import hashlib
import json


def protected_wrapper_calls(records):
    return [
        record
        for record in records
        if record["phase"] in PROTECTED_CALLER_PHASES
        and (record["role"] == "daemon" or record["command"] in STARTUP_CONTROL_COMMANDS)
    ]


def require_no_startup_control_calls(records):
    forbidden = protected_wrapper_calls(records)
    if forbidden:
        raise RuntimeError(
            "managed-runner or inspection startup control command observed: " + json.dumps(forbidden, sort_keys=True)
        )


def compact_sequence_evidence(values):
    encoded = json.dumps(values, sort_keys=True, separators=(",", ":")).encode()
    return {"count": len(values), "sha256": hashlib.sha256(encoded).hexdigest()}


PUEUE_CONFIG_SHORT = "-c"
PUEUE_CONFIG_LONG = "--config"
# Exact one-argument help and version probes, verified against Pueue 4.0.4.
STANDALONE_PUEUE_PROBES = frozenset({"--version", "--help", "-V", "-h"})


def leading_pueue_config(argv):
    """Split the leading config of one traced Pueue call: return (config or None, remaining arguments).

    This is the acceptance trace contract, not a Pueue argument parser. A call
    whose first argument is not an option has no leading config; a later
    config is ordinary payload. A call that is exactly one standalone help or
    version probe has no config. Every other option-first call must start
    with a supported config form: ``-c C``, ``-cC``, ``--config C`` or
    ``--config=C``. Any other option-first layout, including ``-c=C``, raises
    ValueError as an unsupported trace shape, as does a config without a
    non-empty value. The value is literal text: it is never resolved against
    the filesystem.
    """
    arguments = tuple(argv)
    if not arguments or not arguments[0].startswith("-"):
        return None, arguments
    if len(arguments) == 1 and arguments[0] in STANDALONE_PUEUE_PROBES:
        return None, arguments
    first = arguments[0]
    if first in (PUEUE_CONFIG_SHORT, PUEUE_CONFIG_LONG):
        config, remaining = (arguments[1], arguments[2:]) if len(arguments) > 1 else ("", ())
    elif first.startswith(PUEUE_CONFIG_LONG + "="):
        config, remaining = first[len(PUEUE_CONFIG_LONG) + 1:], arguments[1:]
    elif first.startswith(PUEUE_CONFIG_SHORT) and not first.startswith(PUEUE_CONFIG_SHORT + "="):
        config, remaining = first[len(PUEUE_CONFIG_SHORT):], arguments[1:]
    else:
        raise ValueError(
            "unsupported option-first Pueue call layout; a traced call starts with its subcommand, "
            "one standalone help or version probe, or -c C, -cC, --config C or --config=C"
        )
    if not config:
        raise ValueError("leading Pueue config option has no value")
    return config, remaining
