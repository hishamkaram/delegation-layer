#!/bin/sh
set -u
umask 077

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
artifact_bin=${DELEGATE_ACCEPTANCE_BIN_DIR:-$repo_root/bin}
passed=0
blocked=0
failed=0
run_case() {
    case_provider=$1
    case_permission=$2
    case_scenario=$3
    set +e
    set --
    if [ "$case_provider" = ccr:print ]; then
        set -- --model "${CCR_LIVE_MODEL:-}"
    fi
    if [ -n "${DELEGATE_ACCEPTANCE_BIN_DIR:-}" ]; then
        set -- "$@" --pueue "$artifact_bin/pueue" --pueued "$artifact_bin/pueued"
    fi
    python3 "$repo_root/scripts/acceptance_native.py" "$@" \
        --provider "$case_provider" \
        --permission "$case_permission" \
        --scenario "$case_scenario" \
        --delegate "$artifact_bin/delegate" \
        --runner "$artifact_bin/delegate-run"
    result=$?
    set -e
    case "$result" in
        0) passed=$((passed + 1)) ;;
        2) # User-authorized authentication/prerequisite blocks remain neutral.
            blocked=$((blocked + 1))
            ;;
        *) failed=$((failed + 1)) ;;
    esac
}

for provider in antigravity:print claude:print ccr:print codex:exec pi:json opencode:run; do
    run_case "$provider" workspace-write write
done
for provider in antigravity:print claude:print ccr:print codex:exec pi:json opencode:run; do
    run_case "$provider" read-only read-only
done
run_case codex:review read-only read-only
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
printf 'acceptance-native aggregate status=%s passed=%s blocked=%s failed=%s auth_blocks_neutral=true\n' \
    "$aggregate_status" "$passed" "$blocked" "$failed"
exit "$aggregate_exit"
