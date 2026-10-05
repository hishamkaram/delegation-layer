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
skill_file="$(CDPATH= cd -- "$script_dir/.." && pwd)/SKILL.md"
DELEGATE_ROUTING_EVENT="$event" DELEGATE_ROUTING_RULE="$rule" DELEGATE_ROUTING_SKILL="$skill_file" awk '
BEGIN {
    # Paths are task data, not shell code. Escape every JSON control character.
    for (i = 1; i < 32; i++) escapes[sprintf("%c", i)] = sprintf("\\u%04x", i)
    escapes["\\"] = "\\\\"
    escapes["\""] = "\\\""
    path = ENVIRON["DELEGATE_ROUTING_SKILL"]
    for (i = 1; i <= length(path); i++) {
        character = substr(path, i, 1)
        escaped = escaped ((character in escapes) ? escapes[character] : character)
    }
    # The maintained rule is a JSON string. Keep its escaping when prefixing it.
    printf "{\"hookSpecificOutput\":{\"hookEventName\":\"%s\",\"additionalContext\":\"Read the complete agent-integration skill at %s. %s}}\n", ENVIRON["DELEGATE_ROUTING_EVENT"], escaped, substr(ENVIRON["DELEGATE_ROUTING_RULE"], 2)
}'
