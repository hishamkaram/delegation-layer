# Connect Delegate to a Gateway

Use a trusted, self-hosted [Delegation Layer Gateway](https://github.com/hishamkaram/delegation-layer-gateway)
to reach this device from an MCP client. Delegate remains the task execution,
supervision and result owner. Gateway authenticates callers and routes requests;
it does not replace Delegate's task engine.

Install a compatible Delegate release on the device. The Gateway operator first
deploys HTTPS and device connectivity, configures identity and owner policy, and
enables enrollment. Follow the [Gateway deployment guide](https://github.com/hishamkaram/delegation-layer-gateway/blob/main/docs/deployment.md)
for those server-side steps. Examples below use an example hostname and existing
local directories; replace them with your own.

## Browser or headless login

```sh
delegate gateway connect https://gateway.example.com
```

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
delegate gateway connect https://gateway.example.com \
  --token-file /absolute/private/enrollment.json

delegate gateway connect https://gateway.example.com --token-file - \
  < /absolute/private/enrollment.json
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

The profile fixes the selected installed executable's canonical path. A package
upgrade that removes that path needs a reviewed offline migration; this feature
does not transparently replace the executable or rebind saved source identity.
No in-place source migration command is provided. Preserve the root, profile,
credentials and task history, and review the migration before removing the
selected installation. Ordinary Delegate upgrades retain their existing behavior.
A newer installed Delegate can still inspect, stop or uninstall the exact saved
service and request credential revocation. Those cleanup commands use the
current executable as their helper without changing the saved service source.
New installation or startup refuses a missing or redirected saved executable.

An absent root is initialized with the required connector features through a
create-only operation. An existing root is never silently upgraded. If connection
reports `root_cutover_required`, first stop older Delegate writers/frontends and
drain their tasks. Then explicitly enable the feature on that same selected root:

```sh
delegate context enable-connector --root /absolute/delegate/state \
  --workspace-constrained --offline-cutover-confirmed --json
```

This is a one-way cutover: older strict Delegate versions refuse the upgraded
root. The confirmation flag records your assertion that old writers stopped; it
cannot prove their termination. Do not use this flag merely to dismiss an error.
See the [connector cutover contract](connector-contract.md#context-and-explicit-cutover).
Retry the original connection after completing the cutover.

## Approve an existing workspace once

No workspace is approved by default. Explicitly approve an existing local
directory, such as a parent holding several projects:

```sh
delegate gateway workspace allow /absolute/work --alias work
delegate gateway workspace list --json
```

Approval records that directory's canonical path. Replacing or recloning the
ancestor or a project at the same path is allowed. Approval does not scan
projects, create directories or clone repositories. The default alias is the
canonical directory's basename when it is a valid alias; otherwise supply
`--alias`.

Within this approved ancestor, a Gateway client can select an existing relative
project, for example `team/service`. The connector asks Delegate to resolve and
pin that exact project before admission. Creating another child project does not
require approving the same ancestor again. Stable project handles retain the
same canonical path across replacement and policy edits. Each new command
selects the current directory at that path; an already running command keeps
its original working directory. Replacement or removal alone does not invalidate
its accepted result. Missing paths, traversal and escaping symlinks refuse new
launches. A handle never switches devices or canonical paths.

The path-binding feature requires `connector-path-project-v1` on the selected
Native root. New Gateway roots enable it at creation; an existing root needs the
explicit offline cutover described above. Existing physical-identity approvals
keep their original checks. Replace such an approval explicitly once to adopt
path semantics; restarting the service needs no new approval or project handle.
The Native root guard remains strict: if a host restart changes its observed
filesystem identity, connection fails closed and needs explicit operator
reconciliation. No task history or saved guard is silently rewritten.

Approval has a local provider, permission and budget ceiling. Connection defaults
to eligible provider profiles compiled into this Delegate installation,
`read-only`, a maximum of 60 seconds, and each profile's permitted native model
and effort defaults. This describes allowed requests, not installed-provider or
login readiness. Select a narrower ceiling explicitly when connecting:

```sh
delegate gateway connect https://gateway.example.com --headless \
  --provider claude:print --permission read-only --max-budget 60s
```

An explicit `--model` or `--effort` also requires one explicit `--provider`.
Profiles requiring an option are excluded from the implicit default unless it
has been supplied explicitly. Gateway's owner policy and the local workspace
ceiling must both permit a request. Neither a workspace approval nor read-only
provider mode is an OS sandbox; trusted providers retain their native account
and filesystem access.

Read the workspace ID and revision from `workspace list`. Revise its alias or
ceiling with that exact revision:

```sh
delegate gateway workspace update WORKSPACE_ID --expected-revision 1 \
  --max-budget 60s
delegate gateway workspace remove WORKSPACE_ID --expected-revision 2
```

Use the actual returned revision rather than assuming these example numbers.
Concurrent edits fail explicitly. An update preserves omitted policy fields and
cannot retarget the approved directory. Removal disables fresh work and retains
history and frozen pins for safe status, result retrieval and cancellation. It
does not cancel existing tasks or delete their results.

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
reports a qualified busy profile or an unconfirmed connector stop. Joined
storage, identity, manager or cleanup faults remain operational failures, even
when the stop also remains unconfirmed. Inspect the response;
command success alone is not a provider readiness or task closure claim.

## Per-user service and disconnect

Normal connection manages a Linux user systemd service or a macOS LaunchAgent.
It requires the selected user's available service manager. On macOS, run service
management from that user's logged-in graphical Terminal session. An SSH session
does not substitute for the required Aqua session. There is no tmux setup step,
sudo fallback or automatic service-manager repair.
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

Unverified: the new connection, dynamic-workspace and service flow still needs
the integrated live acceptance checks for the release being installed. Unit,
fixture and race checks do not establish real browser authorization, provider
authentication, service persistence or cloud-client compatibility. Consult the
Gateway's [verification record](https://github.com/hishamkaram/delegation-layer-gateway/blob/main/docs/verification.md)
for the exact tested release and environment.
