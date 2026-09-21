# Troubleshooting

Use `--json` while diagnosing a task. It keeps admission, liveness, and
publication separate and includes a bounded error message without embedding
provider answer text.

## Provider is not found or rejected before admission

Check that the executable is on the dispatch process's `PATH`, is a regular
executable file, and can run in the requested workspace. Then run the native
help command yourself. Delegation Layer invokes the provider's version and help
commands and requires every adapter flag to appear in help output. A missing
flag or an unreadable executable is a capability failure, not a release-version
failure.

The observed version is evidence for diagnostics and task binding only. It is
not checked against a hardcoded release, semver range, web lookup, or binary
allowlist. If a probe is rejected, inspect the bounded reason class in the
control response and compare the selected CLI's current help output with the
adapter's required flags.

Use `delegate providers --json` to confirm the profile ID and required runtime
flags. Discovery does not prove that the current host has the executable.

Codex and Claude use the same portable runtime admission path on Linux and
Darwin. If either is rejected, inspect the specific executable, required flag,
policy, or authentication reason in the structured response.

## Authentication fails

Authenticate with the provider's own CLI workflow and retry a new task. Do not
copy credential files into the Delegation Layer state root. Native login caches
and account policy can change between preparation and launch; the provider's
reported authentication result is authoritative for that turn.

For a live acceptance command, an unavailable native login is reported as
`BLOCKED` with exit status `2` after a terminal authentication refusal is
positively identified. The receipt is sanitized and the aggregate live gate
treats that status as neutral; a non-authentication rejection remains a
failure.

## Mode or option is unsupported

Compare the request with the catalog entry. `antigravity:print` supports
`workspace-write`, `continuation`, and `native-timeout`; `codex:exec`,
`claude:print` and `opencode:run` support both permission modes; `pi:json`
supports read-only mode. Every provider supports continuation; Pi and OpenCode
also expose their documented model and effort options. Pi maps effort to its
native `--thinking` flag, while OpenCode maps it to `--variant`. Unsupported
model, effort, policy, or timeout combinations are rejected before supervisor
admission.

## Supervisor configuration fails

Normal dispatch starts the bundled Pueue client and daemon with a private Unix
socket below the state root. `dispatch`, `status`, `cancel`, and continuation
checks restart that private daemon after a restart when the saved binding still
matches. `collect` may recover that daemon only to observe an already-requested
budget stop; it never starts a provider or submits work. For an advanced
integration, pass an existing absolute `--pueue-config` path or set
`DELEGATE_PUEUE_CONFIG` to that path and keep its configuration and credentials
outside the task state root and workspace. The runtime accepts a nonempty
observed supervisor version when its command, readiness, and queue behavior
match the supported schema. A changed executable, configuration file, or
resolved supervisor settings can invalidate a saved task binding.

## A task remains pending

`status` reports what is known and may recover the private supervisor. `collect
--watch 5s` waits for one bounded observation interval and may recover the
private supervisor to record termination for a requested budget stop. Queue time
does not consume the provider budget. Do not delete the task directory or reuse
its ID while the state is uncertain.

## A continuation is busy

The predecessor still owns its conversation reservation, or its runner has not
released it after a completed outcome or durable budget-stop termination. Use
`status` or `collect` until the response exposes `continuation.resumable: true`,
then retry the continuation with the exact predecessor ID.

## A result is rejected

A rejected outcome is a terminal result with exit code 4. Inspect the outcome
and sealed raw descriptors from `collect` and `logs`. Common causes are changed
provider output shape, session identity mismatch, an invalid
declared artifact, or a provider-reported failure. The task is not retried
automatically. A native configuration error does not by itself prove that the
user's settings are wrong: an obsolete adapter-supplied command override can
also cause it. Preserve the evidence and check the installed delegate version;
do not delete provider settings or MCP configuration as a generic repair.

## Collection fails after a successful run

If the response contains a valid outcome alongside an operational error, keep
the outcome: it is independently committed. Re-run `collect` or `logs` to
retry descriptor validation and reservation cleanup. Preserve the state root
for inspection when the error remains unresolved.
