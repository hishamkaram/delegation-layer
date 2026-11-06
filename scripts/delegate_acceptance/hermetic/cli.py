"""Parse arguments for the finite supervisor acceptance harness."""

from scripts.delegate_acceptance.hermetic.suite import HermeticSuite
from scripts.delegate_acceptance.hermetic.installation import REPOSITORY
import argparse


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--source-root", default=str(REPOSITORY))
    parser.add_argument("--tools", help="retired; use --source-root to build owned fixtures")
    parser.add_argument("--output", required=True)
    parser.add_argument(
        "--case",
        action="append",
        choices=(
            "C05",
            "C05-before-start",
            "C07-locked-unknown",
            "C08-ended-unsealed",
            "SESSION-CHAIN",
            "SESSION-CHAIN-BASELINE",
            "SCHEDULER-COMPAT",
        ),
        dest="cases",
        help="run one or more focused compiled H controls instead of the complete suite",
    )
    args = parser.parse_args()
    if args.tools is not None:
        parser.error("--tools is retired; use --source-root to build owned fixture installations")
    suite = HermeticSuite(args.source_root, args.output)
    if args.cases:
        suite.run_targeted(args.cases)
    else:
        suite.run()
