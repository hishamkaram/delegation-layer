# Task CLI

These commands manage durable provider tasks. Antigravity has a certified
workspace-write profile on its recorded version/platform. Codex has a certified
read-only profile for 0.154.0 on Darwin/arm64. Claude remains planned until its
adapter phase passes acceptance.
The finite `fixture:test` launch profile is compiled only into acceptance programs.

```text
delegate [--root ABS] [--pueue-config ABS] [--runner ABS] dispatch \
  --provider PROFILE --brief FILE --cwd ABS [--id TASK_ID] \
  [--permission MODE] [--budget DURATION] [--model MODEL] \
  [--effort EFFORT] [--native-timeout DURATION] [--resume-task PREDECESSOR_ID] [--json]

delegate [--root ABS] status TASK_ID [--json]
delegate [--root ABS] collect TASK_ID [--watch DURATION] [--json]
delegate [--root ABS] logs TASK_ID [--json]
delegate [--root ABS] cancel TASK_ID [--json]
```

Task IDs are 32 lowercase hexadecimal characters. Omit `--id` to allocate a
new ID. Reusing an ID requires the same immutable request and never grants
another provider turn. A resume request allocates a new task and names its
exact predecessor; it does not select a session by recency.

A continuation reserves that conversation until it has a validated terminal
outcome and its runner has released ownership. Active or uncertain execution
keeps the reservation. If releasing a completed task's reservation fails,
the command returns an operational error while preserving its outcome;
collecting that task again retries the release.

The brief is a finite file of at most 8 MiB. It is passed through stdin, not
placed in the provider's argument list. The task budget defaults to `30m` and
must be a positive Go duration such as `30s` or `5m`. It begins immediately
before the one provider Start attempt and includes process wait and pipe
capture. Queue time does not consume it. Token and dollar ceilings and raw
provider arguments are unsupported.

Antigravity also accepts `--native-timeout DURATION`, a positive duration no
greater than `--budget`. It sets agy's own print timeout while the supervisor
continues to enforce the full wall-clock budget. When omitted, the print timeout
equals that budget. Other providers reject this option. For example,
`--budget 120s --native-timeout 3s` exercises native timeout handling with time
left to capture and seal the provider's output. This setting is part of the
immutable task request and cannot be changed when reusing a task ID.

`--watch` bounds one collection call and defaults to `0s`. It does not change
the task budget or stop the worker. Collection reads a valid existing outcome
or interprets complete sealed evidence through its recorded predicate. It
never launches, retries, or resumes provider work.

## Provider discovery

`delegate providers --json` describes compiled provider support with
`schema_version: 1` and a `providers` array sorted by provider ID. Each entry
lists supported request options and certified permission/profile, native
version, platform and predicate metadata. Historical-only interpreters are
retained for collection and omitted from discovery.

Discovery reads compiled metadata. It does not create task state, contact
pueue, inspect native authentication or launch a provider. A listed profile is
not a claim that the current host has the required executable or configuration;
dispatch still checks those prerequisites. Unsupported explicit options fail
before supervisor admission. An explicit `--effort default` retains the native
provider-default selection where supported; it does not request a custom effort
level or change the stored request representation. The discovery response has no task admission,
liveness or publication fields.

The Codex profile uses existing personal ChatGPT file authentication in the
native Codex home. Managed policy, alternate authentication, nonempty effective
system/project configuration, explicit model/effort overrides and native timeout
are outside the certified profile and fail before admission. User configuration
is ignored by the pinned native launch flags. Exact conversation continuation
uses `--resume-task TASK_ID`; it allocates a new task. See
[Codex profile evidence and limits](CODEX-ACCEPTANCE.md) for the source inventory,
runtime exclusions and executed containment checks.

## Supervisor configuration

Initial dispatch requires an absolute `--pueue-config` path or the explicit
`DELEGATE_PUEUE_CONFIG` environment setting. There is no default-queue fallback.
The supported supervisor version is pueue 4.0.4. The initial client executable
is resolved once and its absolute path, version, binary hash, exact configuration
bytes, and resolved settings are bound to the task. Later operations use that
saved binding and refuse incompatible fresh authority.

Configuration accepts ordinary YAML and JSON-form YAML, with strict known
fields, bounded aliases and explicit base-section selection. Merge keys,
duplicate keys, custom tags, ambiguous relative paths, and multiple documents
are refused. Defining a profile does not select it. Configuration is limited
to 1 MiB, depth 64, 16,384 original nodes, 128 aliases and 65,536 expanded nodes.

`delegate-run` normally resolves beside `delegate`. The absolute `--runner`
override supports source builds. It receives the saved root and task ID from
pueue and reconstructs the recorded launch profile. Existing admission/start
guards prevent another attempt after an uncertain process result.

## Results and observations

| Exit | Meaning |
|---|---|
| 0 | Command completed; successful collection returned a committed outcome. |
| 1 | Operational, storage, configuration-binding, or output error. |
| 2 | Invalid request or unsupported initial profile/configuration. |
| 3 | Collection is still pending or cannot yet establish a terminal outcome. |
| 4 | Collection returned a valid rejected outcome. |

`--json` returns a versioned control object with separate `admission`, `liveness`
and `publication` fields. `outcome`, when present and validated, is the terminal
authority. Payload and raw output are file descriptors in this response, not
embedded answer text. Inspect the exit code and any `error` alongside the
outcome: an ancillary operational error does not erase an independently valid
published result.

Each complete JSON response is limited to 1 MiB, including JSON escaping and
the final newline. Oversized diagnostic text is shortened with
`error_truncated: true` or the affected stop's `message_truncated: true`.
Authority fields and descriptors are retained. If those fields alone exceed
the limit, the command returns exit 1 before writing JSON.

`logs` identifies the two absolute raw-stream paths. Live descriptors report
availability without a final size or hash. Sealed descriptors include verified
byte sizes and SHA-256 hashes. Collection remains usable for a valid historical
winner when the provider executable, working directory or external supervisor
configuration is no longer available.

Cancellation records one explicit request, then freshly verifies the exact
saved task before asking pueue to stop one running job or remove one queued
job. It does not broaden or automatically repeat an uncertain request. A
request, acknowledgment and observed job end are separate facts. None alone
creates a provider seal or outcome, and a supervised job's end does not prove
that every escaped descendant ended. Cancelling a valid terminal outcome is
a no-op.

## Verification

`make check` runs the build, static checks and unit/race gates.
`make acceptance-protocol` runs the inherited compiled protocol matrix.
Install the pinned supervisor test pair with
`scripts/install-test-supervisor.sh`, then run `make acceptance-supervisor`.
The latter builds acceptance programs around the shared application and runs
finite fake-client cases followed by one private pueue/pueued lifecycle.
Its real lifecycle uses no workload stop/kill/remove operation; it shuts down
the private daemon only after positive natural completion. Evidence is written
under `bin/phase2-acceptance/`; unresolved failures preserve private paths and
PIDs for inspection.

### Local Antigravity acceptance authentication

AGY selects a different authentication flow when it detects SSH. If a local terminal or tmux environment retains `SSH_CONNECTION`, a supervised headless run can report authentication required even though an interactive local session works. On the verified local host, `env -u SSH_CONNECTION make acceptance-agy` selected the existing local keyring credentials. This is a process-local diagnostic workaround, not a production environment rewrite. Do not apply it blindly to a genuine remote SSH session. See [AGY authentication](https://antigravity.google/docs/cli/install/).
