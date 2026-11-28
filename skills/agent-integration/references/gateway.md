# Gateway and guarded local connectors

Use the [Gateway connection guide](https://github.com/hishamkaram/delegation-layer/blob/main/docs/gateway-connect.md)
for browser/headless connection and workspace approvals. Preserve saved root
and profile choices under the main skill's [root selection rule](../SKILL.md#3-prepare-separate-paths-and-a-finite-brief).
Ordinary local, SSH, Teams and scheduler commands remain in the
[command reference](command-reference.md).

## Guarded local connectors

Operator-managed connectors use the public guarded context, resolve, result,
filesystem, models and origin contracts described in the
[connector contract](https://github.com/hishamkaram/delegation-layer/blob/main/docs/connector-contract.md).
Native alone validates provider/model/effort/permission/budget choices. Connection
and workspace approval save routing, not policy seeds or execution ceilings.
New preparation requires an explicit positive budget duration or `unlimited`.
Unlimited requires Native capability `execution-budget-v1`. Other omissions
retain Native defaults. Model catalogs and capability options are advisory.

For historical task controls, `--expected-project-sha256` compares SHA256 of the
exact saved token bytes instead of requiring the vanished project's raw token.
It is not a fresh project selection and cannot authorize fresh dispatch or
scoped model discovery. Preserve the original root, origin and spec guards.

Guarded Native continuation accepts a distinct stable successor ID and fresh
successor origin:

```sh
delegate continue --root ABS --task PREDECESSOR --budget 10m --id SUCCESSOR --brief /absolute/brief.txt --expected-root ROOT_TOKEN --expected-project-sha256 PROJECT_TOKEN_SHA --expected-origin /absolute/private/predecessor-origin.json --expected-spec-sha256 PREDECESSOR_SPEC_SHA --origin /absolute/private/successor-origin.json --json
```

Keep successor ID and both immutable origins across uncertainty. Native inherits
the frozen project/workspace constraint. Gateway implements `prepare_continuation`;
live continuation and checkpoint acceptance with installed providers remain
unverified. Do not substitute another dispatch or claim unsupported cloud
continuation parity. These candidate contracts have no implied full-provider, native macOS,
new-origin/recovery or cloud acceptance. Ordinary task creation, continuation
and recovery rules in the [command reference](command-reference.md) remain unchanged.

## Scoped model discovery

An already selected Gateway connector uses Native's saved path approval. It
keeps the root/project tokens local and supplies the exact workspace token bytes
through a private regular file (not JSON, no added newline):

```sh
delegate models --root ABS --provider PROVIDER_ID --cwd CANONICAL_PROJECT --expected-root ROOT_TOKEN --expected-workspace-file /absolute/private/workspace-token --relative-project team/service --json
```

The three guards are required together; the file is bounded to 1 MiB. Native
derives the path binding. A supplied cwd must match it; omitting cwd derives
it. Do not decode/regenerate a token or replace this with a historical project
SHA. Physical approvals are not accepted by this new route. Each native command
starts through the existing project-aware lifetime; custody/finish errors cannot
become model facts. Setup and work share the saved absolute inspection deadline.
Ordinary `models`, task budgets and local/SSH/Scheduler/TUI defaults are unchanged.

## Filesystem observations

Use the connector's returned scope ID and a relative selector for new cloud
browsing; it maps those values to Native's saved local approval. No manual alias
or permanent project pin is needed. Approve an ancestor once; new child projects
need no new approval. Whole `/tmp` is automatic on Linux. macOS resolves user
temp through trusted `getconf DARWIN_USER_TEMP_DIR` and includes shared
`/tmp`/`/private/tmp`, with canonical deduplication. A lookup failure stays an
explicit discovery error.

The public local CLI accepts a strict JSON regular file or stdin:

```sh
printf '%s\n' '{}' | delegate filesystem scopes --request - --json
delegate filesystem list --root ABS --request /absolute/private/list.json --json
delegate filesystem read --root ABS --request /absolute/private/read.json --json
delegate filesystem inspect --root ABS --request /absolute/private/inspect.json --json
delegate filesystem git --root ABS --request /absolute/private/git.json --json
```

Requests are at most 1 MiB; `--control-timeout` defaults to `30s` and cannot
exceed `30s`. Pure scopes `{}` needs no root. Other operations retain the root
guard. Lists have at most 100 entries; reads return at most 65536 requested
regular-file bytes. Round-trip opaque cursors/versions and base64 names exactly.
Hidden files, hard links, non-UTF-8 names and binary requested content are not
filtered or secret-scanned. Special entries can be listed/inspected, but not
read as content. Symlinks stay within the same selected scope. Git inspection
reports structural `candidate`/`partial` evidence, never definitive validation.
See [observation results](result-handling.md#filesystem-and-model-observations)
and the [local request contract](https://github.com/hishamkaram/delegation-layer/blob/main/docs/connector-contract.md#read-only-filesystem-observations).

## Connector profile upgrade

For an existing connector profile, review the preview before an authorized
migration:

```sh
delegate gateway upgrade --preview --json
delegate gateway upgrade --accept-execution-policy-removal --json
```

The acceptance flag authorizes removing obsolete saved execution policy. The
command uses existing Native and
OS service owners, compares the saved source before replacement, and preserves
registration, credentials, original root, disabled approvals/revisions, pins and
history. Follow blockers; do not replace state, copy an executable or repeat
login as a shortcut. See the [upgrade guide](https://github.com/hishamkaram/delegation-layer/blob/main/docs/gateway-connect.md#native-root-and-profile).

On macOS, accepted maintenance automatically runs in the same user's existing
GUI domain, including calls from SSH. It preserves the validated service PATH,
or a pending invocation's saved PATH after uninstall. It copies no other SSH
environment or credentials. Preview remains passive. Pending reconciliation
retains the exact Native operation; uncertain invocation/cleanup does not
authorize another launch. A clean `gateway_upgrade_not_started` permits only a
later explicit same-profile invocation after exact-job cleanup; preserve the
saved operation and PATH. `gateway_upgrade_review_required` preserves Native's
`stop_and_report`, never retry guidance. Follow the reported next action for
the same profile; see the [upgrade recovery guide](https://github.com/hishamkaram/delegation-layer/blob/main/docs/troubleshooting.md#gateway-upgrade-and-scope-errors).
Same-path replacement still uses Native's installation upgrade procedure.

## Gateway root context repair

```text
delegate gateway repair-context [--profile ABS] [--json]
delegate gateway repair-context [--profile ABS] --confirm CHALLENGE --offline-cutover-confirmed [--json]
```

Prepare is passive; confirmation explicitly accepts a same-path/same-inode
device-number change for the original root. Follow the
[repair procedure and response decisions](delegate-upgrade.md#explicit-gateway-root-context-repair)
for exact root/profile selection, human attestation, compatibility and failure
handling. Never auto-confirm or retry uncertain work.
