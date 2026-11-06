"""Command entry for finite supervisor acceptance."""

from pathlib import Path
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from scripts.delegate_acceptance.hermetic.case import (
    Case as Case,
)
from scripts.delegate_acceptance.hermetic.cli import (
    main,
)
from scripts.delegate_acceptance.hermetic.scenarios_admission import (
    run_a06 as run_a06,
)
from scripts.delegate_acceptance.hermetic.suite import (
    HermeticSuite as HermeticSuite,
)


if __name__ == "__main__":
    main()
