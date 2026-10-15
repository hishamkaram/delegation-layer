#!/bin/sh
set -eu
umask 077

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
artifact_bin=${DELEGATE_ACCEPTANCE_BIN_DIR:-$repo_root/bin}
passed=0
blocked=0
failed=0
aggregate_stamp=$(date -u +%Y%m%dT%H%M%SZ)-$$
aggregate_output_root=${DELEGATE_ACCEPTANCE_OUTPUT_ROOT:-$HOME/.local/state/delegation-layer-evidence/native-aggregate-$aggregate_stamp}
if [ -e "$aggregate_output_root" ] || [ -L "$aggregate_output_root" ]; then
    printf 'acceptance-native output root already exists: %s\n' "$aggregate_output_root" >&2
    exit 1
fi
mkdir -p "$aggregate_output_root"
chmod 700 "$aggregate_output_root"
aggregate_output_root=$(CDPATH= cd -- "$aggregate_output_root" && pwd)
case_root=$aggregate_output_root/cases
matrix_path=$aggregate_output_root/case-matrix.tsv
mkdir -p "$case_root"
chmod 700 "$case_root"
printf 'case_id\tprovider\tpermission\tscenario\texit_code\tstatus\tcase_output\n' >"$matrix_path"
case_number=0
case_plan=$aggregate_output_root/case-plan.tsv
python3 "$repo_root/scripts/native_acceptance_evidence.py" list-cases >"$case_plan"
expected_case_count=$(wc -l <"$case_plan" | tr -d ' ')
run_case() {
    case_id=$1
    case_provider=$2
    case_permission=$3
    case_scenario=$4
    case_number=$((case_number + 1))
    case_output=$case_root/$case_id
    python3 "$repo_root/scripts/native_acceptance_evidence.py" prepare-case \
        --aggregate-root "$aggregate_output_root" --case-id "$case_id" >/dev/null
    set --
    if [ "$case_provider" = ccr:print ] && [ -n "${CCR_LIVE_MODEL:-}" ]; then
        set -- --model "${CCR_LIVE_MODEL:-}"
    fi
    if [ "$case_provider" = opencode:run ] && [ -n "${OPENCODE_LIVE_MODEL:-}" ]; then
        set -- "$@" --model "$OPENCODE_LIVE_MODEL"
    fi
    if [ -n "${DELEGATE_ACCEPTANCE_BIN_DIR:-}" ]; then
        set -- "$@" --pueue "$artifact_bin/pueue" --pueued "$artifact_bin/pueued"
    fi
    set -- "$@" --output "$case_output"
    if python3 "$repo_root/scripts/acceptance_native.py" "$@" \
        --provider "$case_provider" \
        --permission "$case_permission" \
        --scenario "$case_scenario" \
        --delegate "$artifact_bin/delegate" \
        --runner "$artifact_bin/delegate-run" </dev/null; then
        result=0
    else
        result=$?
    fi
    case "$result" in
        0)
            passed=$((passed + 1))
            case_status=PASS
            ;;
        2) # User-authorized authentication/prerequisite blocks remain neutral.
            blocked=$((blocked + 1))
            case_status=BLOCKED
            ;;
        *)
            failed=$((failed + 1))
            case_status=FAIL
            ;;
    esac
    printf '%s\t%s\t%s\t%s\t%s\t%s\tcases/%s\n' \
        "$case_id" "$case_provider" "$case_permission" "$case_scenario" "$result" \
        "$case_status" "$case_id" >>"$matrix_path"
}

tab=$(printf '\t')
while IFS="$tab" read -r planned_id planned_provider planned_permission planned_scenario; do
    run_case "$planned_id" "$planned_provider" "$planned_permission" "$planned_scenario"
done <"$case_plan"
if [ "$case_number" -ne "$expected_case_count" ] || [ "$case_number" -eq 0 ]; then
    printf 'acceptance-native case plan was not completely consumed\n' >&2
    exit 1
fi
evidence_status=0
evidence_exit=0
helper_python_version=
if helper_python_version=$(python3 -c 'import sys; print(sys.version_info[0])' 2>/dev/null); then
    if [ "$helper_python_version" = 3 ]; then
    require_pass_flag=
    if [ "${DELEGATION_REQUIRE_LIVE_ACCEPTANCE:-0}" = 1 ]; then
        require_pass_flag=--require-pass
    fi
    if [ -n "$require_pass_flag" ]; then
        if python3 "$repo_root/scripts/native_acceptance_evidence.py" aggregate \
            --aggregate-root "$aggregate_output_root" \
            --matrix "$matrix_path" \
            --receipt "$aggregate_output_root/aggregate-receipt.json" \
            --retention-count "${DELEGATE_ACCEPTANCE_RESOLVED_RETENTION:-3}" \
            --tag "${DELEGATE_ACCEPTANCE_TAG:-}" \
            --source-revision "${DELEGATE_ACCEPTANCE_SOURCE_REVISION:-}" \
            --require-pass; then
            evidence_exit=0
        else
            evidence_exit=$?
        fi
    else
        if python3 "$repo_root/scripts/native_acceptance_evidence.py" aggregate \
            --aggregate-root "$aggregate_output_root" \
            --matrix "$matrix_path" \
            --receipt "$aggregate_output_root/aggregate-receipt.json" \
            --retention-count "${DELEGATE_ACCEPTANCE_RESOLVED_RETENTION:-3}" \
            --tag "${DELEGATE_ACCEPTANCE_TAG:-}" \
            --source-revision "${DELEGATE_ACCEPTANCE_SOURCE_REVISION:-}"; then
            evidence_exit=0
        else
            evidence_exit=$?
        fi
    fi
    case "$evidence_exit" in
        0|1|2) ;;
        *) evidence_status=1 ;;
    esac
    if [ ! -f "$aggregate_output_root/aggregate-receipt.json" ]; then
        evidence_status=1
    elif python3 "$repo_root/scripts/native_acceptance_evidence.py" validate \
        --receipt "$aggregate_output_root/aggregate-receipt.json" \
        --tag "${DELEGATE_ACCEPTANCE_TAG:-}" \
        --source-revision "${DELEGATE_ACCEPTANCE_SOURCE_REVISION:-}" >/dev/null; then
        :
    else
        validation_status=$?
        evidence_status=1
        printf 'acceptance-native evidence validation failed exit=%s\n' "$validation_status" >&2
    fi
    else
        evidence_status=1
    fi
else
    probe_status=$?
    evidence_status=1
    printf 'acceptance-native evidence helper probe failed exit=%s\n' "$probe_status" >&2
fi
if [ "$failed" -ne 0 ]; then
    aggregate_status=failed
    aggregate_exit=1
elif [ "$passed" -eq 0 ]; then
    if [ "${DELEGATION_REQUIRE_LIVE_ACCEPTANCE:-0}" = 1 ]; then
        aggregate_status=BLOCKED
        aggregate_exit=2
    else
        aggregate_status=BLOCKED
        aggregate_exit=0
    fi
else
    aggregate_status=passed
    aggregate_exit=0
fi
if [ "$evidence_status" -eq 1 ] || [ "$evidence_exit" -eq 1 ]; then
    aggregate_status=failed
    aggregate_exit=1
elif [ "$evidence_exit" -eq 2 ]; then
    aggregate_status=BLOCKED
    aggregate_exit=2
fi
printf 'acceptance-native aggregate status=%s passed=%s blocked=%s failed=%s auth_blocks_neutral=true\n' \
    "$aggregate_status" "$passed" "$blocked" "$failed"
printf 'acceptance-native case-matrix=%s expected_cases=%s\n' "$matrix_path" "$expected_case_count"
printf 'acceptance-native aggregate receipt=%s\n' "$aggregate_output_root/aggregate-receipt.json"
exit "$aggregate_exit"
