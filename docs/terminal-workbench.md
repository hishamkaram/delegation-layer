# Terminal workbench

Run `delegate tui` in a terminal to browse and control Tasks, Teams, Schedules
and System resources. It calls the same application owners as the CLI; opening
it does not dispatch work, start a scheduler, or repair a supervisor.

```sh
delegate tui
```

The header shows the resolved state root, current project directory and build
version. The ordinary CLI default root applies. For an intentional separate
store, use `delegate --root /absolute/state tui`. The Team catalog keeps its
own configuration scope; changing the execution root does not select a Team.
See [root selection](cli-reference.md#advanced-explicit-and-historical-roots).

Terminal stdin **and** stdout are required. Redirected input/output fails with
exit `2`, before entering the terminal interface. Use ordinary commands with
`--json` for scripts; `tui` has no JSON mode. These docs describe current main;
check `delegate help tui` in your installed build before using it.

## Navigation and forms

| Key | Action |
| --- | --- |
| `Tab` / `Shift-Tab`, or `1`–`4` | Switch view. |
| Up / Down, or `k` / `j` | Select a row. |
| `Enter` | Open details. |
| `a` | Open available actions for the view and selected row. |
| `/`, then `Enter` | Apply metadata search; `Esc` cancels editing. |
| `n` / `p` | Next / previous page, when available. |
| `r` | Refresh the current view. |
| `d` / `o` | Show invocation diagnostics / completed operation results. |
| `?` | Show key help; another key closes it. |
| `Esc` | Return from details, actions or forms. |
| `q` outside an editor, or `Ctrl-C` | Begin orderly quit. |

Forms use `Tab` / `Shift-Tab` to change field, `Ctrl-O` to show advanced
options, and `Ctrl-S` to review. Review shows the exact resource, workspace,
provider, requested model, permission and budget before `Enter` submits.
`Esc` returns to editing. Text inputs keep `q` as text. Long previews scroll
with arrow keys. Terminals narrower than 110 columns use list/detail navigation;
wider terminals show a split view. Below 38 columns or 12 rows, resize to
continue.

Task previews resolve effective Team defaults through the same resolver used
by admission. If the selected Team changes before admission, the operation
refuses the changed selection; review the new values rather than resubmitting
blindly. The UI does not substitute another provider, model or permission.

## What each view controls

| View | Actions |
| --- | --- |
| Tasks | Local/origin-reference history; exact-ID inspection, collection, published result preview, log descriptors and bounded log content, cancellation, interaction events/replies/steering, any/all wait, local continuation and eligible offline recheck; local and remote dispatch forms. |
| Teams | User/project scoped list and definitions, register/update/delete, project default show/set/clear. Definition inputs support a file or inline JSON. Updates/deletes carry the observed revision. |
| Schedules | List, definition, health, history, execution and quota policy; create, pause, resume and delete. Creation supports existing Team selection, failure-hook and timeout options. |
| System | Compiled providers/capabilities, explicit model discovery, doctor/preflight, devices list/probe/trust, supervisor and scheduler controls, managed workspaces, checkpoints and remote data controls. |

The [command reference](cli-reference.md) and its linked contracts own operation
semantics. Unsupported remote continuation/recheck are disabled with a reason.
Use **Select Team project** to choose the explicit project directory for Team
listing and new forms. Outside Git, also supply its containing non-Git project
root. Selection is validated before it changes this view; the summary shows the
chosen context. Project row actions retain the exact directory/root they were
opened from, even if you switch later. This changes only the session view, not
the execution root or saved configuration. A Team revision conflict requires
reloading and reviewing the current definition.
Schedule definitions remain immutable; pause/delete do not cancel admitted tasks,
and resume clears an active hold under the existing scheduler rules. Host
readiness and per-schedule health stay separate.

Scheduler `serve` / `run` are foreground commands, shown as guidance rather
than launched by a UI button. Start the host in an independent terminal or tmux
pane using the [scheduler walkthrough](getting-started.md#recurring-tasks).
Supervisor `ensure-owner` still requires a prepared persistent user context
outside task-owned scope. Native provider settings, accounts, authentication
and permissions stay in the provider's own tools; the TUI adds no settings store.

Device probe/trust and live remote inspection require explicit actions; browsing
never starts background SSH. Verify the exact fingerprint independently before
confirming trust. Remote retirement previews and applies the selected
origin/destination/both scope through the existing active-use checks.

Workspace exec takes **one argument per line**, including the executable on the
first line. One final newline ends the last argument; each additional trailing
blank line encodes an empty argument. Interior empty arguments and literal
spaces/quotes are preserved. It does not parse a shell command or expand variables. It retains
the existing finite budget and captured output. Release, forget, remove,
retirement, migration and stop actions require reviewing the resource and scope;
the UI does not add a general filesystem cleanup command.

## Task history is a dated search view

History searches task metadata across the selected execution root, including
existing origin remote references. It does not discover every task on another
machine. Search covers IDs, provider, requested model/effort, workspace, Team,
Member and remote device. Example:

```text
codex publication:committed order:observed
```

Choose `order:created` (default) or `order:observed` to sort by validated creation
time or the original last-observed timestamp. Switching search/order resets the
page cursor. Observation order never supplies a missing creation date.

Supported status filters are `admission:`, `liveness:`, `publication:` and
`status:`. They match **last-observed catalog values**, which can differ from a
new visible-row observation. Admission, liveness and publication remain
independent. Passive evidence can leave liveness `undetermined`; the UI does not
infer running from a missing outcome. Use explicit **Inspect live status** when
you need the existing status command's live supervisor/remote checks.

A completed metadata discovery pass may still include reported entry faults; it
does not certify that every record is healthy. A consumer cleanup failure before
database closure keeps the handles and allows retrying that cleanup. A physical SQLite close failure instead retains custody
and requires process restart; a second close cannot prove the driver retried.

The display reports indexed, observed, unobserved and fault counts. While
metadata discovery is incomplete, ordering means recent **among indexed
records**, not globally newest. Records without a validated creation time stay
explicitly unknown rather than borrowing filesystem or observation times.
Discovery and observations advance in bounded batches, including observation
progress during a long initial metadata scan. This is eventual coverage, not an
atomic snapshot or a guaranteed refresh latency. Teams and Schedules search
only their loaded page; their summaries say so.

Visible rows refresh passively while browsing; forms/details retain their current
view. A read failure preserves the last valid observation and shows the fault.
Remote rows use the origin's recorded observation time until explicit live
inspection. Cached rows never authorize mutations: actions validate through the
existing source owners.

### Cache ownership and privacy

The disposable history database lives outside the execution root:

- Linux: `$XDG_CACHE_HOME/delegation-layer/history/v1/`, or
  `~/.cache/delegation-layer/history/v1/` when unset.
- macOS: `~/Library/Caches/delegation-layer/history/v1/`.

It uses a root-path-derived filename and validates the source root identity.
Directories are private (`0700`); the database and sidecars are private (`0600`).
It retains metadata and bounded status/failure codes, **no prompts, results,
provider logs, credentials or environment contents**. SQLite may use its own
temporary storage during large scans; the cache-directory mode does not define
system temporary-directory policy.

One elected session writes the catalog. Other sessions read committed pages
and maintain in-memory visible observations. Refresh task discovery requests
another scan; Rebuild disposable history cache replaces only this owned cache,
and defers while another session holds it open. Neither action deletes source
tasks. Invalid source identity blocks further effects until the context is
reopened. An unavailable cache is shown explicitly; exact-ID actions remain
available. A newly absent execution root is never initialized just for browsing.

Published results and raw logs open only through explicit bounded viewers.
Logs are private task content and are never indexed. Terminal control sequences
from stored/provider text are escaped or stripped; viewing text does not execute
shell commands or populate the clipboard. See [security](security.md).

## Quitting and unresolved operations

Switching views, closing forms or quitting does not cancel an admitted task or
stop the independently owned scheduler/supervisor. Cancellation is an explicit
task action. Orderly quit stops new UI work, cancels cooperative reads and waits
for operations that may change state or retain cleanup ownership. Those
operations keep their existing deadlines; the UI adds no deadline to dispatch
or continuation.

If an operation remains unresolved, the closing screen lists it. Press `f`, then
`Enter` to force exit, or `Esc` to keep waiting. After terminal restoration the
CLI prints resource references and supported inspection commands on stderr.
Preserve those references and inspect the same operation; do not automatically
retry an uncertain admission. If cleanup custody is retained, further effects
are blocked and the interface reports that a restart is required.

The first `SIGINT`, `SIGTERM` or `SIGHUP` begins orderly shutdown with a
30-second grace; a second signal or expiry takes the force path. Terminal
restoration cannot be guaranteed after `SIGKILL` or terminal disappearance.
External `SIGQUIT`, `SIGSTOP` and `SIGTSTP` are not supported restoration paths;
there is no suspend key.

| TUI exit | Meaning |
| --- | --- |
| `0` | Normal completed quit. |
| `1` | Runtime, terminal or cleanup error. |
| `2` | Invalid arguments or non-terminal input/output. |
| `3` | Forced exit with unresolved UI operations; inspect printed references. |
| `128 + signal` | Signal-triggered exit (`130`, `143`, `129` for INT, TERM, HUP). |

These are TUI exits, separate from [collect's pending/rejected
exits](../skills/agent-integration/references/result-handling.md#exit-codes).
