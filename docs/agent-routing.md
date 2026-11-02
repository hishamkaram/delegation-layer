# Prefer Delegate for external collaboration

The Claude and Codex plugin supplies a short, provider-neutral routing rule
through native session and subagent hooks. It asks the model to load the skill
before selecting external tools. Startup, resume, clear, and compaction use the
host's SessionStart event; SubagentStart supplies the same preference to native
subagents. No shell interception or provider restriction is added.

Native subagents remain available. A separate external session uses Delegate,
even when it has the same brand as the current harness. An explicit user choice
of another integration takes precedence. Merely discussing or configuring a
provider does not request delegation.

## Activation and status

Choose the plugin or installer-managed hooks in the canonical
[installation guide](installation.md). Inspect the selected installer scope:

```sh
npx --yes delegation-layer routing status --scope global
```

Use `--scope project` for project-local hooks. Global Claude configuration honors
`CLAUDE_CONFIG_DIR`; Codex hooks honor `CODEX_HOME`. Project hooks use
`.claude/settings.local.json` and `.codex/hooks.json`. Global hooks use the active
Claude `settings.json` and Codex `hooks.json`. The helper and rule are installed
with the skill; they need only a POSIX shell and standard local utilities, not
Node or a network connection at runtime.

**Codex requires hook trust.** Open `/hooks` in Codex, review and trust the
installed definitions, then start or resume a session. New or changed definitions
need review again. Project trust and host hook settings also apply. The installer
never grants trust or overrides a host-disabled hook. Its `configured` status
means matching entries exist, not that they ran or that the model followed them.
Check actual native hook events and tool calls for activation evidence.

Ordinary routed commands omit `--root`, allowing the CLI to resolve the same
OS-default store used by a user-started `delegate scheduler serve` from any
project directory. A workspace selects where the task runs. An explicitly
supplied or already recorded nondefault root is preserved for that work.
See [state-root selection and upgrade guidance](cli-reference.md#advanced-explicit-and-historical-roots).

After updating the integration through its normal installation path, verify the
skill and routing content the harness actually loads, including plugin caches
and any older personal skill that takes precedence. Repository changes alone
do not update an installed copy. Old `DELEGATE_ROOT` exports no longer select a
root in corrected skill commands; use the exact explicit old root to access
historical work.

Existing noninteractive installer commands and the standard `skills add` path
remain skill-only. `--routing skill-only` does not remove previously configured
hooks. The installer never installs the CLI as part of skill setup; the plugin's
separate bootstrap or [CLI installation](installation.md#cli-installation)
provides it.

## Disable or recover

```sh
npx --yes delegation-layer routing disable --scope global --harness claude,codex
```

This removes only matching hooks owned by that installer location, retaining
unrelated hooks and settings. Disable project hooks separately with
`--scope project`. Plugin hooks belong to the host's plugin manager: disable or
uninstall the plugin there. Do not install both paths unless you intend to manage
both sources; the status command describes the selected installer scope only.

Settings are validated before installation writes routing entries. Each changed
configuration receives a private backup beside it, then an atomic replacement.
Concurrent installer writes are locked. If a later host update fails, earlier
updates are rolled back when their contents still match; a rollback conflict is
reported with its path rather than overwriting someone else's changes. Skill
files can remain installed after a routing failure. Malformed settings and
symlinked configuration paths are reported without rewriting them; `--force`
only authorizes replacing skill files. An interrupted installer can leave a lock;
verify no installer is running before removing the reported stale lock file.

## Other hosts and limits

Universal, Cursor, Gemini, OpenCode, Pi, and Hermes currently receive skill-only
guidance. The installer reports `unsupported-host` for automatic routing there;
it does not edit global instructions, identity files, or provider settings.

For Hermes, use the active profile's global skill directory. A project-only
skill is not sufficient for builds that discover skills from `HERMES_HOME`.
Users may configure persistent routing preferences through their host's supported
instruction mechanisms, but that is separate from verified automatic setup.

Hooks deliver guidance, not guaranteed model compliance. A correct blocked
attempt reports Delegate's error without silently substituting another
integration or claiming external work completed. Parallel work keeps each
request's response and task ID separate. A debate requires collected results
and actual challenge rounds, not just two dispatch attempts.
