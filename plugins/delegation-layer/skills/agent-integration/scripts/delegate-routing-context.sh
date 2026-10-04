#!/bin/sh
# Context delivery only: no installation, discovery, or provider execution.
set -eu
case "${1:-}" in
    SessionStart|SubagentStart) event=$1 ;;
    *) printf '%s\n' 'Expected SessionStart or SubagentStart.' >&2; exit 2 ;;
esac
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
rule_file="$script_dir/../references/routing-rule.json"
# Read before emitting anything so a missing asset cannot produce partial JSON.
rule=$(cat "$rule_file")
printf '{"hookSpecificOutput":{"hookEventName":"%s","additionalContext":%s}}\n' "$event" "$rule"
