"""Exercise private supervisor reuse across two installed CLI layouts."""

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[3])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]





from scripts.acceptance.private_cli.application import main


if __name__ == "__main__":
    raise SystemExit(main())
