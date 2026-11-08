# Remote delegation

Remote tasks use an embedded Go SSH connection to another prepared installation.
The destination owns admission, native supervision, execution budget, cancellation
and sealed outcomes. The origin retains immutable intent and dated observations;
its connectivity or downloaded files cannot change the destination's outcome.

## Prepare a destination

Install Delegate and its runner/supervisor on the destination. Install and
configure the intended provider under the remote user. Delegate does not install
providers, copy accounts or change host services. Make the installed `delegate`
visible to noninteractive SSH execution, or supply `--remote-executable` with an
absolute path containing only letters, digits, slash, period, underscore, plus
and hyphen.

Start `delegate supervisor ensure-owner --json` from a persistent user context
on that machine. Remote admission requires an authenticated resident; on Linux
it refuses a resident in a `session-*.scope` or a `user@UID.service` whose
account lacks existing systemd linger state. It only observes placement and
linger state; it does not configure either. Native acceptance establishes
logout survival on the tested host, not every session manager. Reboot can
interrupt work; reconnect never restarts a provider.
If the host cannot resolve a user-manager UID to its account name, the linger
check refuses that placement. This includes directory-service accounts absent
from the account lookup available to a static build.

Configure a literal alias in the originating user's SSH configuration. Listing
is passive; probing contacts only the selected device:

```sh
delegate devices list --json
delegate devices probe --device worker --provider claude:print \
  --remote-cwd /srv/project --json
```

Provider preparation is distinct from actual authenticated provider execution.
Preflight reports the resolved provider executable when static preparation
identifies it. The destination's SSH environment selects that installation.
Use only hosts and provider contexts validated for the intended workload.

## SSH support

| Setting | Behavior |
| --- | --- |
| User then system configuration | First obtained scalar/list value wins |
| `Host`, `Include` | Literal aliases; `*`, `?`, negation; sorted conditional includes |
| Include relative paths | User `~/.ssh`, system `/etc/ssh`; depth 16, 128 files, 4 MiB |
| Hostname, user, port, identities, agent, known-host files/alias | Supported bounded subset; `IdentityFile` is additive |
| Authentication | Supported file keys or Unix agent; encrypted file keys need an agent |
| Unknown key | Explicit fingerprint enrollment required |
| Changed/revoked key | Refused |
| `Match`, jump/proxy routing, executable helpers, canonicalization | Explicit unsupported-setting errors |
| Unsupported algorithm/authentication/trust settings | Refused rather than ignored |
| Windows | Unverified |

Use `--ssh-config /absolute/supported.conf` on device commands and dispatch to
select one explicit user configuration instead of the default user/system files.
Missing explicit files fail. The selected path is saved with task intent, so
observation and controls reload it without another flag. Includes retain the
user configuration root. This is an explicit source choice, not a fallback.

This is not complete OpenSSH configuration compatibility. A stock configuration
containing an unsupported setting can be refused; diagnostics identify its
source. No fallback to an external SSH/SCP/SFTP command occurs. No agent
forwarding or credential synchronization occurs.
On the tested stock Ubuntu 24.04 origin, system `Host *` settings include
unsupported `SendEnv` and `GSSAPIAuthentication`; `--ssh-config` is required.
Use an explicit supported file on any origin with those system settings.

After verifying a host fingerprint independently:

```sh
delegate devices trust --device worker --fingerprint SHA256:EXPECTED --json
```

Enrollment writes Delegate's own trust file under its execution root, leaves
user SSH files untouched, and serializes conflicting enrollments.

## Dispatch and files

Fresh remote dispatches check destination CPU and memory pressure by default.
The independent `--cpu-guard=false` and `--memory-guard=false` overrides require
the destination's advertised support before upload or admission. They are not
saved with intent and do not change existing-only reconciliation or a durably
rejected reference. Applying different guards after rejection requires a new
task ID; never replay uncertain work to change its guard selection.

Use exactly one workspace mode:

```sh
delegate dispatch --device worker --provider claude:print \
  --brief /absolute/brief.txt --remote-cwd /srv/project --json

delegate dispatch --device worker --provider claude:print \
  --brief /absolute/brief.txt --input-manifest /absolute/inputs.json \
  --output-manifest /absolute/outputs.json --json
```

`inputs.json` explicitly selects regular files:

```json
{"source":"/absolute/local/project","paths":["src/main.go","notes.txt"]}
```

Selected dirty and untracked files are included. Parent directories are created
implicitly; selecting a directory does not request recursive synchronization.
The exact snapshot bytes are retained before admission. Limits are 10,000 files,
1 GiB total and 256 MiB per file. Symlinks, hardlinks, special files, unsafe paths,
case collisions and reserved control/Git paths are refused. Only the executable
bit is preserved; modes are normalized to 0644/0755. Do not select credentials,
provider settings or installed executables.
If preparation stops after recording its manifest but before freezing all
bytes, later source edits can make that incomplete preparation conflict. Use
a new task ID; an existing manifest never silently selects newer bytes.

`outputs.json` is a JSON list of relative files, for example:

```json
["reports/result.txt","src/main.go"]
```

Declared files are captured after canonical closure, including cancellation or
expiry, under a separate finite auxiliary budget. Their receipt records capture
time and task/input provenance. A prepared remote workspace is externally
mutable, so capture does not claim exclusive provider authorship. Missing files
and capture errors do not change the provider outcome.

## Observe, reconnect and collect

Keep the returned `remote:ID` handle intact:

```sh
delegate status remote:ID --json
delegate events remote:ID --json
delegate collect remote:ID --watch 30s --out /absolute/downloads --json
delegate cancel remote:ID --json
delegate remote reconcile remote:ID --json
delegate remote list --json
```

Clarification replies and steering use the existing commands with that handle
and their original message IDs. Support remains provider-capability dependent.
Remote continuation, logs and offline recheck are not exposed in v1. Teams,
schedules and local-workspace options do not implicitly target remote machines.

Transient connection failures receive at most five total attempts, with 1, 2, 4
and 8 seconds of backoff inside the command deadline. Every connection verifies
trust and recorded destination identity. Authentication, trust, protocol and
request conflicts stop without retry.

An SSH command exit distinguishes a missing remote Delegate (127), a
non-executable entry point (126), and other endpoint failures without exposing
remote stderr. A malformed acknowledgment after a possibly sent admission is
still unknown, even when its protocol error is not retryable. A valid framed
response remains evidence even if the entry point exits unsuccessfully later.

Uncertain acceptance returns exit 3. When the destination explicitly reports
`not_created`, such as an early missing-resident refusal, the origin records a
definite rejection. Correcting that refusal requires a new reference. Repeating
a rejected reference never admits it again.

A destination resource refusal can instead preserve `task_record: unknown`
because remote requests carry an explicit task ID. Its nested envelope reports
`resource_pressure` and `stop_and_report`; exit 3 does not make it a pending-only
response. Report the failure and stop this attempt. Do not loop reconciliation
or create another reference automatically. Reconciliation remains existing-only
and cannot turn that refusal into a new admission.

After admission might have been sent, reconnection only reconciles the original
immutable request. Missing or unreadable remote history remains unknown. It does
not permit fresh admission, another task ID or failover. Transfers resume bytes;
controls retain IDs. No retry renews the provider's execution budget.

The response keeps the authoritative remote envelope separate from connectivity,
last-observation time and file-download reports. An unavailable connection does
not mean failed execution or completed cancellation. An acknowledged cancellation
is distinct from confirmed native closure.

Downloads go beneath the explicit destination in a private task-specific
subdirectory. Existing unowned or replaced task directories are refused.
If an owned task directory was removed, choose a new `--out` directory.
Partial files remain distinguishable from verified files; publication never
overwrites unrelated files. Applying or merging changes is a separate decision.

Ordinary commands omit `--root`; it selects originating state only when supplied.
`--remote-root` explicitly selects destination state. Subsequent operations retain
that exact resolved destination, root and task. Host/user/root changes never
silently retarget work. A changed native root pin permits only verified passive
reads and downloads, with a diagnostic; mutations are refused.
Root identity is checked before and after operations. Output and snapshot
directories are task-specific siblings of the root and use rooted component
access. These checks do not protect against malicious same-user replacement
and restoration within one operation; the user already owns the stored state.

## Retention

The origin reference owns its retained snapshot; the destination task owns its
staged workspace and captured outputs. These are retained history, with no
automatic garbage collection in v1. A snapshot can retain both staging bytes
and a materialized workspace, so budget disk space for both copies per host.

Manual retirement is an operator action after canonical task closure and
completed capture, with no concurrent transfers or further use of that handle.
The task-specific byte directories sit beneath
`.<root-name>.remote-snapshots/<root-id>/<task-id>`,
`.<root-name>.remote-inputs/<root-id>/<task-id>`, and
`.<root-name>.remote-outputs/<root-id>/<task-id>` beside the corresponding root.
Retire only the selected task's bytes and matching `remote-refs`, `remote-inputs`
or `remote-captures` journal beneath that root; retain ordinary task/result
evidence and unrelated tasks. Removing these bytes forfeits further transfer
or recovery from that reference. No cleanup command is implied by task expiry.

Selector validation rejects case collisions but does not normalize Unicode.
Different Unicode spellings that collide on the destination filesystem fail
during materialization without overwriting the existing file.
