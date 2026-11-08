# Integration API

The supported integration surface is `delegate` and its versioned JSON output.
Go packages under `internal/` are private implementation details.

## Request flow

Use the [command reference](../skills/agent-integration/references/command-reference.md)
and [captured manual example](../skills/agent-integration/examples/read-only-task.md).
Apply [task creation](../skills/agent-integration/references/result-handling.md#task-creation)
before observing or retrying any request.

## Task control response

The packaged [task response contract](../skills/agent-integration/references/result-handling.md#task-control-response)
owns the envelope, authority fields, and descriptor rules.
[JSON examples](../skills/agent-integration/examples/json-results.md) show abbreviated shapes.

## Remote responses

Remote task controls can return an origin envelope with the destination reply
under `remote`; `remote.response` carries the original task envelope when
available. Connectivity, dated
`last_observation` and download reports are separate observations. Do not apply
local task-field decisions to the outer origin envelope or infer task failure
from connection loss. Before a task envelope exists, a validated destination
reply can instead carry `remote.failure` and `remote.task_record`. Check
connectivity and concrete errors first; a zero-value `remote` object after a
connection failure is not destination evidence.
The [remote workflow](remote-delegation.md#observe-reconnect-and-collect)
owns remote handles, uncertainty, supported controls and transfer behavior.

## Checkpoint response

See the [checkpoint response contract](../skills/agent-integration/references/workspaces.md#checkpoint-response)
and [decisions](../skills/agent-integration/references/result-handling.md#checkpoint-results).

## State model

See [state dimensions](../skills/agent-integration/references/result-handling.md#task-control-response).
Queue addresses are not task identities.

## Offline answer recovery

See [offline recovery](../skills/agent-integration/references/result-handling.md#offline-recovery).

## Discovery API

See [providers and capabilities](../skills/agent-integration/references/command-reference.md#providers-and-capabilities).

## Model discovery contract

See [model response fields and exits](../skills/agent-integration/references/command-reference.md#choosing-models-and-effort).

## Capability contract

See [capability projections](../skills/agent-integration/references/command-reference.md#providers-and-capabilities).

## Idempotency and continuation

See [single-use task identity](../skills/agent-integration/references/command-reference.md#dispatch)
and [continuation](../skills/agent-integration/references/continuation.md).

## Collection contract

See [observation and result decisions](../skills/agent-integration/references/result-handling.md#observe-and-collect).

## Operator integration limits

Ordinary local task/control JSON responses are bounded to 1 MiB.
[Saved-schedule detail](../skills/agent-integration/references/command-reference.md#schedules)
responses containing the saved request and
[Team management responses](../skills/agent-integration/references/command-reference.md#teams)
have a 2 MiB bound. Task/schedule envelopes can shorten oversized diagnostic
text to fit their bound while retaining authority fields and descriptors.
Native event streams and answer files have their own evidence bounds; they
are not embedded as unrestricted text in the control envelope.

A client must preserve the exit status separately from the JSON and tolerate
optional fields. Syntax errors may print usage on stderr without a JSON response.
For advanced external supervisor or worker integrations, use
[supervisor binding](cli-reference.md#supervisor-binding).
The [architecture](architecture.md) describes implementation ownership;
it is not a public Go API.
