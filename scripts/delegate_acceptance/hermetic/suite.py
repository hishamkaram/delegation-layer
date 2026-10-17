"""Compose independently owned finite cases and publish their summary."""

from pathlib import Path
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.hermetic.case import Case
from scripts.delegate_acceptance.hermetic.chain import run_session_chain
from scripts.delegate_acceptance.hermetic.chain import run_session_chain_baseline
from scripts.delegate_acceptance.hermetic.contract import PARENT_EXIT_BUDGET
from scripts.delegate_acceptance.hermetic.contract import PARENT_EXIT_CHILD_LIFETIME_MS
from scripts.delegate_acceptance.hermetic.files import canonical_executable
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a01
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a02_duplicate
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a02_empty
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a02_malformed
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a03_config
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a03_label
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a04_invalid_config
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a04_saved_binding
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a04_symlink_config
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a04_valid
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a05_concurrent
from scripts.delegate_acceptance.hermetic.scenarios_admission import run_a06
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b01_invalid_budget
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b01_raw_argv
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b01_valid
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b02
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b03
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b04
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b05
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b06_complete_first
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b06_expire_first
from scripts.delegate_acceptance.hermetic.scenarios_budget import run_b07
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c01_mismatch
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c02_mismatch
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c02_valid
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c03_queued
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c03_transition
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c04
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c05
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c05_before_start
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c06
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c07_locked_unknown
from scripts.delegate_acceptance.hermetic.scenarios_control import run_c08_ended_unsealed_cancel
from scripts.delegate_acceptance.hermetic.scenarios_publication import run_p01
from scripts.delegate_acceptance.hermetic.scenarios_publication import run_p02
from scripts.delegate_acceptance.hermetic.scenarios_publication import run_p03
from scripts.delegate_acceptance.hermetic.scenarios_publication import run_p04
from scripts.delegate_acceptance.hermetic.scenarios_publication import run_p05_fault
from scripts.delegate_acceptance.hermetic.scenarios_publication import run_p05_start
from scripts.delegate_acceptance.hermetic.scenarios_publication import run_p06_abandon
from scripts.delegate_acceptance.hermetic.scenarios_publication import run_p06_overlap
from scripts.delegate_acceptance.hermetic.scenarios_publication import run_p07
import sys
import traceback


class HermeticSuite:
    def __init__(self, tools, output):
        self.tools = Path(tools).resolve(strict=True)
        self.output = Path(output).resolve()
        self.output.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.results = []
        for name in ("delegate", "delegate-run", "provider", "pueue-fake", "harnessprobe"):
            canonical_executable(self.tools / name)

    def run_case(self, case_id, fn, **kwargs):
        case = None
        try:
            case = Case(suite=self, case_id=case_id, **kwargs)
            fn(case)
            result = case.finish("pass")
            self.results.append(result)
            return
        except BaseException as error:
            if case is not None:
                try:
                    case.retain_failure(error)
                except BaseException:
                    pass
            result = {
                "case": case_id,
                "status": "fail",
                "error": str(error),
                "traceback": traceback.format_exc(),
            }
            if case is not None:
                result["private_base"] = str(case.base)
            self.results.append(result)

    def write_summary(self):
        summary = {
            "schema_version": 1,
            "suite": "fake-supervisor-hermetic",
            "tools": {
                name: {
                    "path": str(self.tools / name),
                    "sha256": digest(self.tools / name),
                }
                for name in ("delegate", "delegate-run", "provider", "pueue-fake", "harnessprobe")
            },
            "cases": self.results,
            "counts": {
                "pass": sum(item["status"] == "pass" for item in self.results),
                "fail": sum(item["status"] == "fail" for item in self.results),
            },
            "forbidden_operations": {
                "signals_sent": 0,
                "shell_true": 0,
                "command_context": 0,
                "wait_delay": 0,
                "process_group_operations": 0,
                "real_native_supervisor": 0,
            },
        }
        write_json(self.output / "summary.json", summary)
        for result in self.results:
            if result["status"] == "fail":
                print("FAIL " + result["case"] + ": " + result["error"], file=sys.stderr, flush=True)
                print(result["traceback"], file=sys.stderr, flush=True)
        return summary

    def run(self):
        self._run_admission_cases()
        self._run_publication_cases()
        self._run_budget_cases()
        self._run_control_cases()
        summary = self.write_summary()
        if summary["counts"]["fail"]:
            raise SystemExit(1)
        print(
            "PASS hermetic H: %d cases; A/P/B/C counts and independently started "
            "delegate-run evidence recorded" % summary["counts"]["pass"],
            flush=True,
        )

    def _run_admission_cases(self):
        self.run_case("A01", run_a01)
        self.run_case("A02-empty", run_a02_empty)
        self.run_case("A02-duplicate", run_a02_duplicate)
        self.run_case("A02-malformed", run_a02_malformed)
        self.run_case("A03-config", run_a03_config)
        self.run_case("A03-label", run_a03_label)
        self.run_case("A04-invalid-config", run_a04_invalid_config)
        self.run_case("A04-symlink-config", run_a04_symlink_config)
        self.run_case("A04-valid", run_a04_valid)
        self.run_case("A04-saved-binding", run_a04_saved_binding)
        self.run_case("A05-concurrent", run_a05_concurrent)
        self.run_case("A06", run_a06)

    def _run_publication_cases(self):
        self.run_case("P01", run_p01)
        self.run_case("P02", run_p02, provider_kwargs={"scenario": "large"})
        self.run_case("P03-success", run_p03, provider_kwargs={"scenario": "success"})
        self.run_case("P03-empty", run_p03, provider_kwargs={"scenario": "empty"})
        self.run_case("P03-diagnostic", run_p03, provider_kwargs={"scenario": "diagnostic"})
        self.run_case("P03-malformed", run_p03, provider_kwargs={"scenario": "malformed"})
        self.run_case("P03-truncated", run_p03, provider_kwargs={"scenario": "truncated"})
        self.run_case("P03-nonzero", run_p03, provider_kwargs={"scenario": "nonzero"})
        self.run_case(
            "P04",
            run_p04,
            provider_kwargs={"scenario": "parent_exit", "provider_hold": True, "child_lifetime_ms": 5000},
        )
        self.run_case("P05-start", run_p05_start, hook="start-failed")
        self.run_case("P05-wait", run_p05_fault, hook="wait-failed")
        self.run_case("P05-capture", run_p05_fault, hook="capture-failed")
        self.run_case(
            "P06-overlap",
            run_p06_overlap,
            provider_kwargs={"scenario": "hold", "provider_hold": True, "lifetime_ms": 5000},
        )
        self.run_case("P06-abandon", run_p06_abandon, hook="abandon-before-seal")
        self.run_case(
            "P07",
            run_p07,
            provider_kwargs={"scenario": "late_session", "provider_hold": True, "lifetime_ms": 5000},
        )

    def _run_budget_cases(self):
        self.run_case("B01-invalid-budget", run_b01_invalid_budget)
        self.run_case("B01-raw-argv", run_b01_raw_argv)
        self.run_case("B01-valid", run_b01_valid)
        self.run_case("B02", run_b02, budget="1s")
        self.run_case("B03", run_b03, provider_kwargs={"scenario": "hold", "provider_hold": True, "lifetime_ms": 5000})
        self.run_case(
            "B04",
            run_b04,
            provider_kwargs={
                "scenario": "parent_exit",
                "provider_hold": True,
                "child_lifetime_ms": PARENT_EXIT_CHILD_LIFETIME_MS,
            },
            budget=PARENT_EXIT_BUDGET,
        )
        self.run_case("B05", run_b05, hook="start-delayed", budget="1s")
        self.run_case("B06-complete-first", run_b06_complete_first)
        self.run_case(
            "B06-expire-first",
            run_b06_expire_first,
            provider_kwargs={
                "scenario": "parent_exit",
                "provider_hold": True,
                "child_lifetime_ms": PARENT_EXIT_CHILD_LIFETIME_MS,
            },
            budget=PARENT_EXIT_BUDGET,
        )
        self.run_case(
            "B07",
            run_b07,
            hook="publication-delayed",
            provider_kwargs={"scenario": "hold", "provider_hold": True, "lifetime_ms": 5000},
            budget="1s",
        )

    def _run_control_cases(self):
        self.run_case("C01-mismatch", run_c01_mismatch)
        self.run_case("C02-valid", run_c02_valid)
        self.run_case("C02-mismatch", run_c02_mismatch)
        self.run_case("C03-queued", run_c03_queued)
        self.run_case("C03-transition", run_c03_transition)
        self.run_case("C04", run_c04)
        self.run_case("C05", run_c05)
        self.run_case("C05-before-start", run_c05_before_start)
        self.run_case("C06", run_c06)
        self.run_case("C07-locked-unknown", run_c07_locked_unknown)
        self.run_case("C08-ended-unsealed", run_c08_ended_unsealed_cancel)
        self.run_case("SESSION-CHAIN", run_session_chain)


    def run_targeted(self, case_ids):
        controls = {
            "C05": run_c05,
            "C05-before-start": run_c05_before_start,
            "C07-locked-unknown": run_c07_locked_unknown,
            "C08-ended-unsealed": run_c08_ended_unsealed_cancel,
            "SESSION-CHAIN": run_session_chain,
            "SESSION-CHAIN-BASELINE": run_session_chain_baseline,
        }
        for case_id in case_ids:
            fn = controls.get(case_id)
            if fn is None:
                raise SystemExit("unknown targeted H case: " + case_id)
            self.run_case(case_id, fn)
        summary = self.write_summary()
        if summary["counts"]["fail"]:
            raise SystemExit(1)
        print(
            "PASS hermetic H targeted: %d cases; compiled CLI and natural "
            "process receipts recorded" % summary["counts"]["pass"],
            flush=True,
        )
