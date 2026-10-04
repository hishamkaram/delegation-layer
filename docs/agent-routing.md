# Prefer Delegate for external collaboration

Installing the skill makes it available for discovery. It does not establish a
persistent preference over another integration, and some hosts show the model
only a shortened skill description. A model can therefore attempt commands
without loading the skill's operating instructions.

If you want Delegate to be the default for external collaboration, add the
following rule to your harness's persistent instructions. Keep any existing
instructions. The rule is provider-neutral; the installed skill supplies the
commands and Delegate discovers supported providers.

```text
For external agent or model collaboration (reviews, research, implementation,
testing, second opinions, comparisons, or debates), load the installed
agent-integration skill before choosing tools or issuing commands, then use
Delegate. This includes CCR / Claude Code Router. Do not invent Delegate syntax
or substitute provider CLIs, plugins, or APIs. If Delegate returns
failure.next_action=stop_and_report, report the failure and stop; do not retry,
investigate provider setup, or fall back. Ordinary native subagents within this
harness remain available unless a separate external session is requested.
Respect an explicit user choice of a different integration. Merely discussing
or configuring a provider does not request delegation.
```

## Hermes

Install the skill globally for the active Hermes profile. The tested Hermes
build reads `$HERMES_HOME/skills/agent-integration` (normally
`~/.hermes/skills/agent-integration`); a project-local copy alone is insufficient.

In the active profile's `config.yaml`, append the rule above to the existing
`agent.system_prompt` string. If it is empty, set it to the rule. Keep other
configuration and competing skills intact, then start a new session. This is a
one-time preference; normal requests do not need to mention Delegate.

The installed CLI supports this setting. Hermes's newer documentation also
provides `skills.auto_load`, but older builds may not implement it. Do not add
an unsupported setting and assume the skill was loaded. See the
[Hermes configuration documentation](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/configuration.md).

Persistent instructions improve routing guidance; they do not mechanically
prevent a model from bypassing it. Verify actual tool calls, not a statement that
Delegate was used. A correct blocked attempt reports Delegate's error without
calling the provider directly, retrying a `stop_and_report` failure, or claiming
that the requested external work completed.
