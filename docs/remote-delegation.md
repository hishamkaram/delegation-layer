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

`devices probe` reports separate configuration, connectivity, trust,
authentication, endpoint, root, resident and provider-preparation stages.
Stages remain unknown or skipped when earlier evidence cannot establish them.
Without both `--provider` and `--remote-cwd`, provider preparation is skipped.
Provider authentication always remains unknown: static preparation does not
execute an authenticated provider task. Diagnostics contain codes, source
locations and counts; they do not contain forwarded environment values.

## SSH support

| Setting | Behavior |
| --- | --- |
| User then system configuration | First obtained scalar/list value wins |
| `Host`, `Include` | Literal aliases; `*`, `?`, negation; sorted conditional includes |
| Include relative paths | User `~/.ssh`, system `/etc/ssh`; depth 16, 128 files, 4 MiB |
| Hostname, user, port, identities, agent, known-host files/alias | Supported bounded subset; `IdentityFile` is additive |
| Authentication | Supported file keys or Unix agent; encrypted file keys need an agent |
| `PubkeyAuthentication`, `PreferredAuthentications` | Honor key-authentication selection; refuse before key/agent access if it excludes public-key authentication |
| `GSSAPIAuthentication` | Accept configuration; key/agent authentication remains available, with an explicit diagnostic when selected GSSAPI cannot be used |
| `SendEnv` | Ordered additive patterns and `-pattern` removals select from one captured invocation environment; send before remote exec |
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
Stock Ubuntu `SendEnv LANG LC_*` and `GSSAPIAuthentication yes` settings do not
require `--ssh-config` for key/agent connections. Full Kerberos authentication
is not implemented. An authentication policy excluding public keys fails
explicitly; Delegate never silently switches to password or interactive login.
The remote SSH server may decline selected environment requests; this produces
a count diagnostic and does not by itself fail the connection. Selection is
limited to 128 variables, 32 KiB per entry and 256 KiB total before dialing.
Other system policy remains subject to the supported subset. For example, a
macOS origin selecting `Ciphers` through its system configuration still needs
an explicit supported configuration; Ubuntu default compatibility does not
imply complete OpenSSH configuration support on every host.

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
delegate logs remote:ID --json
```

Clarification replies and steering use the existing commands with that handle
and their original message IDs. Support remains provider-capability dependent.
Remote continuation and offline recheck are not exposed. Teams,
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

## Bounded log snapshots

`logs remote:ID --json` returns metadata for stdout and stderr without raw bytes
or destination absolute paths. Explicitly select a stream to read a snapshot:

```sh
delegate logs remote:ID --stream stdout --offset 0 --limit 65536 --json
delegate logs remote:ID --stream stdout --cursor SAVED_CURSOR --limit 65536 --json
```

The limit defaults to 65,536 bytes and cannot exceed it. `logs.data_base64`
preserves arbitrary bytes; `logs.range` reports the offset, returned length,
observed file size, seal state, range checksum and next opaque cursor. Empty
EOF ranges are valid. A live EOF is only an observation; a later snapshot may
contain appended bytes. There is no follow mode.

A cursor binds the destination root, task, stream and native file object.
Foreign cursors, replacement and truncation fail explicitly. An explicit offset
must agree with a supplied cursor. Sealed whole-stream checksums are expected
manifest metadata; range checksums attest only the returned bytes. Seal and
complete flags do not certify a full reread of the file. Full task/result
evidence keeps its existing validation. Log snapshots neither cache raw bytes
in origin history nor acquire admission, capture or execution authority.
Older destinations lacking the log capability are refused before a log request.

## Storage and explicit retirement

The origin reference owns its retained snapshot; the destination task owns its
staged workspace and captured outputs. These are retained history, with no
automatic garbage collection. A snapshot can retain both staging bytes
and a materialized workspace, so budget disk space for both copies per host.

New byte families use the recorded layout
`.<root-name>.remote-data/<root-id>/<task-id>/{snapshot,inputs,outputs}` beside
the corresponding state root. Origin and destination select their layouts
independently; destination layout is negotiated before its cwd and intent are
fixed. Existing legacy snapshot/input/output directories remain in place and
are read through their recorded layout. No migration is inferred from path
existence.

```sh
delegate remote usage remote:ID --scope both --json
delegate remote retire remote:ID --scope both --dry-run --json
delegate remote retire remote:ID --scope both --apply --json
```

`--scope` accepts `origin`, `destination` or `both` (the default). Usage is
passive and reports observation time, layout, family paths, bytes, entries and
availability. Its walk stops after 10,000 entries per family and marks truncated
observations. Preview is the default for `retire`; `--dry-run` makes it explicit.
It writes no records and never probes execution locks. Preview is advisory;
`--apply` rechecks all requirements while holding the role's exclusive byte
lease. A busy lease or transient-only fault returns exit 3; a concrete fault
remains exit 1, including when joined with a transient error.

Destination retirement requires canonical terminal closure, released execution
and completed declared-output capture. Origin retirement requires a sent or
definitely rejected reference. Newly admitted Delegate tasks whose cwd is inside
a managed family register an immutable dependency before task creation;
retirement also waits for those consumers' canonical closure and capture.
Uncreated, missing or unreadable consumer evidence blocks deletion. Legacy
destination input workspaces report `legacy_inputs_consumers_unknown`; there
is no force option. These checks cover admitted Delegate consumers, not arbitrary
shell processes or tasks admitted by older installations.

Apply records a durable tombstone before deleting only the selected byte
families, then records completion. Retries preserve the tombstone and repeat
incomplete deletion safely. Failure leaves the tombstone in place; correct the
reported filesystem problem and repeat the same operation. No new upload or
managed consumer can adopt retired bytes. Origin references, destination task
records, capture receipts, raw logs, result payloads, downloaded files and a
user-prepared cwd remain intact. Status, collect, reconciliation and bounded
logs remain available; removed captured outputs report `retired` without a new
download. No cleanup command is implied by task expiry. Older destinations
lacking retirement support are refused; origin-only scope remains independent.

Selector validation rejects case collisions but does not normalize Unicode.
Different Unicode spellings that collide on the destination filesystem fail
during materialization without overwriting the existing file.
