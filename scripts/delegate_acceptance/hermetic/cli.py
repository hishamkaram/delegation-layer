"""Parse arguments for the finite supervisor acceptance harness."""

from scripts.delegate_acceptance.hermetic.suite import HermeticSuite
import argparse


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--tools", required=True)
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
        ),
        dest="cases",
        help="run one or more focused compiled H controls instead of the complete suite",
    )
    args = parser.parse_args()
    suite = HermeticSuite(args.tools, args.output)
    if args.cases:
        suite.run_targeted(args.cases)
    else:
        suite.run()
