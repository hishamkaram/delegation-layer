#!/bin/sh
set -u
umask 077

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
passed=0
blocked=0
failed=0
run_case() {
    case_provider=$1
    case_permission=$2
    case_scenario=$3
    set +e
    python3 "$repo_root/scripts/acceptance_native.py" \
        --provider "$case_provider" \
        --permission "$case_permission" \
        --scenario "$case_scenario" \
        --delegate "$repo_root/bin/delegate" \
        --runner "$repo_root/bin/delegate-run"
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

for provider in antigravity:print claude:print codex:exec pi:json opencode:run; do
    run_case "$provider" workspace-write write
done
for provider in antigravity:print claude:print codex:exec pi:json opencode:run; do
    run_case "$provider" read-only read-only
done
if [ "$failed" -ne 0 ]; then
    aggregate_status=failed
    aggregate_exit=1
elif [ "$passed" -eq 0 ]; then
    aggregate_status=BLOCKED
    aggregate_exit=0
else
    aggregate_status=passed
    aggregate_exit=0
fi
printf 'acceptance-native aggregate status=%s passed=%s blocked=%s failed=%s auth_blocks_neutral=true\n' \
    "$aggregate_status" "$passed" "$blocked" "$failed"
exit "$aggregate_exit"
