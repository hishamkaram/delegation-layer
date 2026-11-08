#!/usr/bin/env python3
"""Run the bounded Claude ``print`` acceptance gate.

The canonical provider driver owns state, protocol, and evidence behavior.
This entry point parses arguments and selects the terminal CLI result.
"""

from __future__ import annotations

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[4])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]


import argparse



from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.provider_drivers.claude.runner import ClaudeAcceptance
from scripts.acceptance.provider_drivers.claude.runtime import BlockedFailure


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog='acceptance-claude', description=__doc__)
    parser.add_argument("--tools", required=True, help="directory containing shipped delegate binaries")
    parser.add_argument("--pueue", required=True)
    parser.add_argument("--pueued", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--claude", default=None, help="installed Claude executable override")
    parser.add_argument("--delegate", default=None, help="delegate executable override")
    parser.add_argument("--runner", default=None, help="delegate-run executable override")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        ClaudeAcceptance(args).run()
    except BlockedFailure as error:
        print("BLOCKED: " + str(error), file=sys.stderr, flush=True)
        return 2
    except AcceptanceFailure as error:
        print("FAIL: " + str(error), file=sys.stderr, flush=True)
        return 1
    except BaseException as error:
        print("FAIL: " + str(error), file=sys.stderr, flush=True)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
