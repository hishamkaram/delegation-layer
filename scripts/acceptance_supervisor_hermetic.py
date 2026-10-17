"""Command entry for finite supervisor acceptance."""

from pathlib import Path
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from scripts.delegate_acceptance.hermetic.case import (
    Case,
)
from scripts.delegate_acceptance.hermetic.chain import (
    require_chain_provider_receipts,
    require_chain_session_release,
    require_chain_task_links,
    run_session_chain,
    run_session_chain_baseline,
)
from scripts.delegate_acceptance.hermetic.cli import (
    main,
)
from scripts.delegate_acceptance.hermetic.contract import (
    CASE_TIMEOUT,
    FIXTURE_TIME,
    FIXTURE_VERSION,
    HarnessFailure,
    MAX_CONTROL_BYTES,
    MODE,
    OBSERVATION_BOUND,
    PARENT_EXIT_BUDGET,
    PARENT_EXIT_CHILD_LIFETIME_MS,
    PREDICATE,
    PREDICATE_SHA256,
    SCHEMA_VERSION,
    SUPERVISOR_VERSION,
    add_entry_label,
    canonical_json_bytes,
    label_parts,
    private_pueue_yaml,
    require_true,
    status_document,
    status_job,
    yaml_string,
)
from scripts.delegate_acceptance.hermetic.evidence import (
    require_live_raw_descriptors,
    require_publication,
    require_supervisor_argv,
)
from scripts.delegate_acceptance.hermetic.files import (
    canonical_executable,
    copy_executable,
    owned_directory_path,
    task_id,
    write_bytes,
)
from scripts.delegate_acceptance.hermetic.observations import (
    assert_counts,
)
from scripts.delegate_acceptance.hermetic.scenarios_admission import (
    run_a01,
    run_a02_duplicate,
    run_a02_empty,
    run_a02_fixture,
    run_a02_malformed,
    run_a03_config,
    run_a03_label,
    run_a04_invalid_config,
    run_a04_saved_binding,
    run_a04_symlink_config,
    run_a04_valid,
    run_a05_concurrent,
    run_a06,
)
from scripts.delegate_acceptance.hermetic.scenarios_budget import (
    run_b01_invalid_budget,
    run_b01_raw_argv,
    run_b01_valid,
    run_b02,
    run_b03,
    run_b04,
    run_b05,
    run_b06_complete_first,
    run_b06_expire_first,
    run_b07,
)
from scripts.delegate_acceptance.hermetic.scenarios_common import (
    complete_success_case,
)
from scripts.delegate_acceptance.hermetic.scenarios_control import (
    run_c01_mismatch,
    run_c02_mismatch,
    run_c02_valid,
    run_c03_queued,
    run_c03_transition,
    run_c04,
    run_c05,
    run_c05_before_start,
    run_c06,
    run_c07_locked_unknown,
    run_c08_ended_unsealed_cancel,
)
from scripts.delegate_acceptance.hermetic.scenarios_publication import (
    run_p01,
    run_p02,
    run_p03,
    run_p04,
    run_p05_fault,
    run_p05_start,
    run_p06_abandon,
    run_p06_overlap,
    run_p07,
)
from scripts.delegate_acceptance.hermetic.suite import (
    HermeticSuite,
)


if __name__ == "__main__":
    main()
