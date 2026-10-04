# CCR model selection

Use this reference when asked to consult Claude Code Router (CCR), use one of
its configured model aliases, or compare its models with other harnesses.
CCR runs Claude Code against a configured third-party or local provider.

1. Resolve CCR through `delegate providers --json`; its profile is `ccr:print`.
2. Preserve an explicitly requested CCR alias as `--model ALIAS`. Do not replace
   it with an upstream model ID or reject it solely because discovery omitted it.
3. If no alias was chosen, run
   `delegate models --provider ccr:print --cwd ABSOLUTE_WORKSPACE --json`.
   Choose a suitable configured alias from the available metadata and report
   the choice. Clarify when the intended model remains ambiguous. If discovery
   is unavailable and no alias is known, report the blocker; do not guess.
4. Dispatch through Delegate with `--provider ccr:print --model ALIAS`, the brief,
   workspace, permission, finite budget, and `--json`. Use the normal status,
   collect, and continuation procedure from the main skill.

CCR requires an explicit alias. Unspecified external collaboration still uses
`dispatch --auto`; do not prefer CCR unless the request calls for it. Multiple
aliases can be assigned independent tasks for comparison or debate.

Read-only selects native Claude plan behavior; workspace-write selects native
unattended tool execution. Neither is an independent Delegate sandbox. Preserve
requested effort through `--effort`; unknown effort metadata is not an allowlist
or a reason to invent a level. Leave effort unspecified unless requested.

Delegate uses CCR's foreground provider-only route. Model/provider setup and
credentials belong to CCR. Never run `ccr launch`, configure CCR, inspect its
SQLite database, call Claude directly, or select another integration to bypass a
Delegate error. Ordinary questions about CCR or installation do not trigger
external collaboration. An explicit request to use another integration remains
an explicit user choice.
