# Connect Delegate to a Gateway

Use a trusted, self-hosted [Delegation Layer Gateway](https://github.com/hishamkaram/delegation-layer-gateway)
to reach this device from an MCP client. Delegate remains the task execution,
supervision and result owner. Gateway authenticates callers and routes requests;
it does not replace Delegate's task engine.

This guide describes the prepared Native 0.9.0 / Gateway 0.2.0 candidate, not a published release. Check the installed `delegate help gateway` before using these commands.

Install a compatible Delegate build on the device. The Gateway operator first
deploys HTTPS and device connectivity, configures identity and owner policy, and
enables enrollment. Follow the [Gateway deployment guide](https://github.com/hishamkaram/delegation-layer-gateway/blob/main/docs/deployment.md)
for those server-side steps. Examples below use an example hostname and existing
local directories; replace them with your own.

## Browser or headless login

```sh
delegate gateway connect https://gateway.example.com --name mac-home
```

Connection records identity and routing only. It does not select or save a
provider, model, effort, permission mode or execution budget. Those choices and
their defaults belong to Native for each task. `connect` and `workspace` do not
accept execution-policy flags. Omitted choices stay omitted until Native
applies its defaults; finite Native task budgets remain required.

Use `--name mac-home` to give the device a recognizable display name. Otherwise,
the name defaults to the local hostname. The name can help an MCP client find
the device later; it is not its routing identity.

The command prints the verified login URL and a short user code, then requests
that your desktop open the URL. Finish approval in that browser. A browser
activation failure leaves the same login pending: open the printed URL yourself
and enter the printed code. The activation/chooser request has a five-second
budget. A timeout does not establish that a browser stayed closed. Missing or
unsupported desktop bus access also uses this same printed URL and code;
Delegate does not start a bus or fall back to spawning a browser process.
Approval expires; the command has a ten-minute
deadline. Opening a browser does not establish successful authentication.

On a terminal without a browser, use the same login flow explicitly:

```sh
delegate gateway connect https://gateway.example.com --headless
```

Open the printed URL on another device and enter its code. `--headless` avoids
opening a browser; a person must still approve the login. It is different from
unattended enrollment.

For unattended setup, obtain the private one-time enrollment response through
the Gateway operator's documented token workflow. Store it in an owned regular
0600 file outside the checkout, or provide the JSON through stdin:

```sh
delegate gateway connect https://gateway.example.com --token-file /absolute/private/enrollment.json

delegate gateway connect https://gateway.example.com --token-file - < /absolute/private/enrollment.json
```

Pass the file path or `-`, never the token value as an argument. Enrollment
tokens, certificates and private profiles are not suitable for chat, logs or
source control. A private server CA can be selected with `--ca ABSOLUTE_FILE`;
normal public HTTPS uses the platform trust roots. TLS verification stays on.

After enrollment, connection installs and starts a per-user connector service.
It saves the selected Gateway, installed Delegate executable and Native root
identity in the local profile. It does not approve a workspace or establish
provider authentication. Connecting an existing profile to a different Gateway
fails; use a separate explicit profile for that connection. If the service
manager is unavailable, the command reports failure and retains the completed
enrollment for an explicit retry.

To run in the current terminal instead, opt out of service installation:

```sh
delegate gateway connect https://gateway.example.com --foreground
```

Leave that process running while using the device. For a saved profile, use
`delegate gateway serve` to run the same connector without another login.

## Native root and profile

By default these commands use Delegate's normal configured root resolver. They
do not select a separate task-state directory. The default Gateway profile is
the `gateway` directory inside that resolved root. Use an absolute `--profile`
on each command to select another profile; there is no hidden current-profile
selection. Advanced installations can also select their existing root with
`--root ABSOLUTE_DIRECTORY`.

The profile records the installed executable's canonical path and the original
Native root. Use the supported upgrade owner when handing an existing profile
to a newer installed binary or removing an old saved execution policy:

```sh
delegate gateway upgrade --preview --json
delegate gateway upgrade --accept-execution-policy-removal --json
```

Preview reports profile migration counts and saved/current source selection;
it performs no service or Native command and does not prove readiness. Review
it before explicit acceptance. Preserve the same `--profile` and `--root` on
both commands when using overrides. Quiesce the scheduler and other producers
as described in [installation upgrades](cli-reference.md#installation-upgrades).

Acceptance stops and uninstalls the owned connector service, uses Native's
existing supervisor upgrade owner, verifies the original root and required
public contracts, removes obsolete execution-policy fields, and compares the
saved source before selecting the current installed binary. It then reinstalls
and starts the service. This explicit upgrade also authorizes completion of a
retained, prepared Native handoff using the current installed pair; preview and
status do not. Registration, credentials and their generation, original
root, approvals including disabled entries/revisions, frozen pins and task
history are preserved. No replacement root, copied executable, tmux session or
repeat login is part of this migration.

On macOS, accepted maintenance runs automatically in the same user's existing
GUI session, including when invoked through SSH. Preview stays passive. The
fixed-purpose maintenance job is separate from the normal connector and is not
installed for login autostart. It preserves PATH from the validated
managed service definition. A retained pending invocation preserves that PATH
even after service removal; only an initial invocation without either source
uses the invoking user's PATH. No other SSH environment or credentials are
copied into the GUI session.

Read `upgrade.phase`, `failure`, `next_action` and the Native supervisor
observation together. Missing prerequisites and pending handoffs block progress;
service startup does not prove cloud connectivity or provider readiness. Retry
the same supported command only as its reported next action allows. Do not edit
saved source paths, credentials or journals to bypass a refusal. See
[Gateway upgrade troubleshooting](troubleshooting.md#gateway-upgrade-and-scope-errors).

Reconciliation retains the exact Native operation; it never substitutes a later
operation or relaunches an uncertain invocation. `gateway_upgrade_uncertain`
requires inspection of the same profile. `gateway_upgrade_not_started` means
the maintenance job exited before claiming the upgrade; after its recorded
cleanup, the reported next action permits a new invocation of the same command.
It preserves the saved source, PATH and any exact Native operation. A known
installation-verification failure before Native maintenance also permits an
explicit retry after correcting the installation. Ambiguous historical receipts
remain uncertain; they are never reclassified as proof that nothing changed.
`gateway_upgrade_review_required` preserves Native's `stop_and_report`, even
when Native returned a pending exit code. Keep the reported operation and
inspect its Native blocker; do not retry maintenance. An invalid private
maintenance record reports `gateway_upgrade_record_invalid` and
`stop_and_report`; do not delete records as repair. Same-path executable replacement still uses Native's
[installation upgrade procedure](cli-reference.md#installation-upgrades).
Gateway does not infer replacement from path equality or force a daemon reload.

If only the native device number changed for the same original root path and
inode, use [explicit root context repair](cli-reference.md#root-context-repair).
That preserves the saved profile identity and task history; it does not retarget
the root or installed executable.

An absent root is initialized with the required connector features through a
create-only operation. An existing root is never silently upgraded. If connection
reports `root_cutover_required`, first stop older Delegate writers/frontends and
drain their tasks. Then explicitly enable the feature on that same selected root:

```sh
delegate context enable-connector --root /absolute/delegate/state --workspace-constrained --offline-cutover-confirmed --json
```

This is a one-way cutover: older strict Delegate versions refuse the upgraded
root. The confirmation flag records your assertion that old writers stopped; it
cannot prove their termination. Do not use this flag merely to dismiss an error.
See the [connector cutover contract](connector-contract.md#context-and-explicit-cutover).
Retry the original connection after completing the cutover.

## Approve an existing workspace once

Approve an existing ancestor once, such as the parent holding several projects:

```sh
delegate gateway workspace allow /absolute/work
delegate gateway workspace list --json
```

Approval stores its canonical path and grants routing beneath that ancestor. It
creates no projects or repositories and saves no provider or execution policy.
New child projects need no separate approval. New cloud requests select a
returned `scope_id` and relative project path; they do not require manual aliases
or permanent project pins. `--alias` remains optional local display/legacy
selection metadata.

Native automatically discovers the whole shared `/tmp` scope on Linux. On
macOS it discovers the user's temporary directory using the fixed trusted
`getconf DARWIN_USER_TEMP_DIR` lookup, plus shared `/tmp` (normally canonical
`/private/tmp`), with canonical duplicates removed. These are whole scopes, not
only Delegate-created temporary files. A discovery failure is reported; no
arbitrary environment path is substituted and no manual temp alias is needed.

Within one selected scope, clients can list directories, read requested regular
file bytes and inspect Git structural evidence. Hidden files, hard links,
non-UTF-8 names and binary requested files are supported. No filename/content
filter or secret scanner changes the requested data. Devices, FIFOs and sockets
can be listed/inspected but are never opened as file content. Symlinks must stay
inside that same selected scope, even if another scope is also approved. Git
inspection reports candidates/partial evidence; it does not validate a repository
with Git commands or establish repository trust.

New commands reacquire the current directory at the approved canonical path;
same-path replacement or recloning is allowed. An already running command keeps
its original cwd. Missing paths, traversal and escaping symlinks refuse new
launches. Native root identity stays exact. Older physical approvals keep their
original checks; replace one explicitly once to adopt path semantics. Existing
roots require the [offline cutover](#native-root-and-profile) before path-bound
operations. Approval and provider read-only mode are not an OS sandbox.

Only display metadata can be revised; use the actual returned workspace ID and
revision, rather than assuming these example numbers:

```sh
delegate gateway workspace update WORKSPACE_ID --expected-revision 1 --alias work
delegate gateway workspace remove WORKSPACE_ID --expected-revision 2
```

Updates cannot retarget the approved directory. Removal disables fresh routing
and retains history/frozen pins for observation, retrieval and cancellation. It
does not cancel existing tasks or delete their results. Native alone validates
provider/model/effort/permission/budget choices for every task; discovery is
advisory and never vetoes an explicit model ID merely because it is unlisted.

## Observe credentials and local prerequisites

```sh
delegate gateway status --json
delegate gateway doctor --json
```

Status reports dated local credentials and whether their identity is in use. It
does not infer online connectivity or execution readiness from a certificate.
Doctor checks the saved Native root identity and required feature locally. It
does not probe a provider, repair a login, or claim provider authentication.
Use the MCP client's device and capability tools to observe the actual Gateway
connection, then explicitly select the intended device and project.

Gateway commands support `--json` for a versioned response with explicit
`failure` and `next_action` on error. Exit `0` means that command succeeded,
`1` reports an operational failure, `2` reports invalid arguments, and `3`
reports a qualified busy profile, pending installation handoff or unconfirmed connector stop. Joined
storage, identity, manager or cleanup faults remain operational failures, even
when the stop also remains unconfirmed. Inspect the response;
command success alone is not a provider readiness or task closure claim.

## Select the device and provider in your cloud client

After connecting and approving a workspace, a prompt can name the device:

> On mac-home, browse the approved work scope, select team/service, and ask
> Claude to summarize it read-only with a 60-second budget.

The client calls `list_devices`, finds one unambiguous name and uses its returned
`device_id`. Workspace inventory supplies scopes; file tools take a scope ID and
relative path. `get_capabilities` reports actual Native provider contracts;
explicit `list_models` pages advisory model/effort facts and may perform Native
inspection. It is not run implicitly by capability discovery. Explicit Native
execution options are forwarded to task preparation; omissions retain Native
defaults. No cloud-side model list or connector policy supplies execution
permission.

Names are display metadata. Duplicate names or an offline device require an
explicit device choice; do not guess or switch destinations. Connection and
approval do not establish provider login or task readiness. The paired candidate
uses Gateway `prepare_continuation` followed by `dispatch_task` over Native's
[guarded continuation](connector-contract.md#guarded-native-continuation).
Retain the exact predecessor and successor identities through uncertain
delivery; do not substitute a fresh dispatch.

A cloud connection usable for at least seven days depends on the identity
provider issuing refresh grants and the actual cloud client using them. Device
certificate lifetime and WSS/socket timeouts do not extend a cloud OAuth login.
Follow the Gateway [cloud login guide](https://github.com/hishamkaram/delegation-layer-gateway/blob/main/docs/cloud-login.md);
no tenant changes or seven-day acceptance are implied by these instructions.

## Per-user service and disconnect

Normal connection manages a Linux user systemd service or a macOS LaunchAgent.
It requires the selected user's available service manager. On macOS, Native
targets that same user's existing `gui/UID` domain explicitly. Service management
can run through same-user SSH; the calling shell need not be in Aqua. A missing
GUI domain fails with `gateway_gui_session_required` and
`check_user_gui_session`. Native does not create the login session, select a
background service instead, configure tmux or use sudo. Registration alone does
not prove running, authenticated or execution-ready status.
The service runs within that user manager's lifetime; this does not configure
Linux lingering, a system-wide daemon or a pre-login macOS service.

Use explicit service commands when retrying a failed installation or managing
an existing saved connection:

```sh
delegate gateway service install
delegate gateway service start
delegate gateway service status --json
delegate gateway service stop
delegate gateway service uninstall
```

Service operations use a 30-second bound. Installation is idempotent for the
same profile, source, arguments and safety settings. Source, argument,
extra-setting and unsafe-file changes are refused. A manager registration/start
observation does not prove authenticated
WSS connectivity or provider readiness. No second connector replaces an active
profile owner. Service settings retain only the installation PATH from the
environment; provider credentials remain managed by the provider and operating
system.

The owned service file is the authority for its valid PATH. A canonical PATH-only
edit is accepted; the other settings still require an exact match. A later
terminal's different
PATH does not rewrite that installation or prevent its cleanup. Service-file
publication requires the filesystem's atomic no-replace rename support; an
unsupported filesystem fails explicitly, with no hard-link fallback. Foreign
files and existing units with
extra hard links remain refused. Only a private, profile-specific unpublished
staging file is reconciled by an explicit installation retry.
Installation and removal acknowledge success only after the relevant directory
entries are durably synced. If a filesystem barrier fails, retry the same
operation; a retry also confirms durability when the definition is already
present or already absent. Credentials, profile data and task history remain
preserved.
On Linux, install, stop and uninstall can refresh one matching inactive systemd
cache entry before checking again. The cache never authorizes targeted service controls or
replaces the owned definition. A fresh `not-found` definition can retain failed
exit history; reconciliation does not reset that history. Status only observes.
If Status or Start reports `gateway_service_cache_stale`, run
`delegate gateway service stop` with the same profile selection, then retry.
Start still refuses an absent definition; it does not repair the cache itself.
If a valid definition appears during reconciliation, retry the same profile;
foreign or unsafe definitions remain refused.

`delegate gateway disconnect` stops and uninstalls only this profile's managed
connector service. It preserves credentials, approved workspaces, task history
and results. A pending stop is not reported as confirmed termination. Stopping a
connector does not cancel or prove closure of Delegate-owned provider tasks.
Foreground `serve` can exit nonzero when stopped during a failed dial or
reconnect backoff, because the earlier connection fault remains an error.
An exit code alone is not provider-task closure.
Uninstall retains the service definition when inactivity cannot be confirmed.
For a foreground connector, stop its own foreground process first; disconnect
does not signal an unrelated process merely because it holds the profile lease.

To revoke the remote device credential too, request it explicitly after local
connector inactivity can be confirmed:

```sh
delegate gateway disconnect --revoke
delegate gateway disconnect --revoke --headless
```

Revocation requires a fresh manage-only login and Gateway owner authorization.
The headless form prints the same approval URL and code without opening a
browser. Plain disconnect does not perform this login or revoke credentials.
Revocation preserves the local profile, pins and task evidence; it does not
cancel provider tasks. A failed or uncertain revocation is reported as an error,
so do not infer that the credential was revoked from a local service stop.

The Native disconnect path currently requires an available OS service manager,
including for a profile previously used only in the foreground. If that manager
is unavailable, the foreground process's owner must stop and join it. The
Gateway operator can separately revoke the credential without a browser on the
Gateway host:

```sh
gateway devices revoke --socket /absolute/private/run/admin.sock --device DEVICE_ID
```

See the [Gateway deployment guide](https://github.com/hishamkaram/delegation-layer-gateway/blob/main/docs/deployment.md)
for operator access. Remote credential revocation proves neither local connector
termination nor provider-task closure. It does not delete the device's local
profile or task history.

## Verification limits

The Gateway's [November 14 public Native onboarding checkpoint](https://github.com/hishamkaram/delegation-layer-gateway/blob/main/docs/verification.md#november-14-public-native-onboarding-checkpoint)
records dated live observations and their limits. Match its exact tested
revisions and environments to the release being installed; preparing a release
does not publish it or extend that evidence to other installations. Unit,
fixture and race checks alone do not establish real browser authorization,
provider authentication, service persistence or cloud-client compatibility.

The current [Gateway verification record](https://github.com/hishamkaram/delegation-layer-gateway/blob/main/docs/verification.md)
separates paired-process, native macOS, installed-provider and actual Cloud
evidence by revision. Home's two-turn Native and ChatGPT continuation passed at
the recorded candidate revisions. The automatic Mac maintenance correction,
Office rollout, other-provider coverage and seven elapsed days of Cloud refresh
remain unverified. Earlier results do not establish those later acceptance gates
or candidate release readiness.
