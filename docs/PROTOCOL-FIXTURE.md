# Durable protocol acceptance fixture

`internal/testutil/protocolfixture` is a finite test executable for Phase 1. It uses the actual task store and fake admission/Start sinks. It does not invoke a native provider or supervisor and does not establish native runtime readiness. Build it with `make build`; run the acceptance suite with `make acceptance-protocol`.

A normal lifecycle is `prepare`, `claim -kind admission`, `seal`, then `collect`. `seal` acquires and consumes the Start permit in its own process, records the fake Start entry, captures both streams while holding the runner lease, closes them, and commits `provider.exit`. A separate `claim -kind runner` deliberately ends unsealed and is used only for launch-authority and crash tests. Reopening that task cannot restore its runner capability or seal its unfinished capture.

`seal -publish` additionally finalizes through the registered predicate before releasing its runner lease. This exercises continuous ownership from Start through publication; ordinary `seal` leaves a valid seal for independent recovery.

Every task operation takes an explicit `-root` and `-task-id`. Prepare also requires `-canonical-cwd` and `-brief-file`. The default provider is `fixture:test` in `read-only` mode; this is an explicitly fake binding. Prepare records the canonical paths, normalized requested configuration, registered fixture predicate, actual helper executable/build digest and an explicit fake supervisor configuration. `-budget` is persisted at preparation; omitting it from a later runner operation uses that immutable value. A supplied different value fails before Start.

The pure fixture predicate preserves exact nonempty `raw/stdout` bytes on success and produces a deterministic nonempty refusal on failure. `collect -raw-output` streams the selected payload. Normal collection accepts no answer/verdict replacement. `candidate -file result.txt -content TEXT -verdict committed` is the separate adversarial publication seam; `-candidate-record FILE` supplies a bounded complete outcome candidate for identity/descriptor conflict tests. Valid terminal winner evidence is returned alongside a candidate conflict.

`seal` accepts finite `-raw-stdout`/`-raw-stderr` values or streams from regular `-raw-stdout-file`/`-raw-stderr-file` inputs. Raw answers are not limited to the control-record ceiling. `collect -wrong-predicate` selects an unavailable version to test conservative recovery. `inspect` reads current evidence without changing it. `session -action claim|release -provider fixture:test -conv-id ID` exercises persistent continuation claims; release requires the explicit verified `-evidence-digest`.

`reconcile -observations FILE` reads a bounded array of fake observations, each containing `root_id`, `task_id`, `spec_sha256` and `meta_sha256`. Exactly one matching observation can attach to a task with submission intent. Zero, multiple, malformed or unavailable observations remain unknown. This command performs no admission or Start operation; actual supervisor reconciliation is a later phase.

## Faults and finite process rendezvous

Commands that mutate records accept `-checkpoint`, `-rendezvous`, `-events`, `-release-file` and `-wait-budget` (default 10 seconds). Reaching the exact selected checkpoint writes `<pid>:<checkpoint>\n` and self-exits with code 3 without deferred cleanup. When `-release-file` is supplied, it instead holds ownership until that file appears, within the finite wait budget. Missing or failed observation writes fail the command.

Record boundary names include `before-KIND-link`, `after-KIND-link-before-barrier`, `after-KIND-barrier` and `before-KIND-cleanup`. KIND identifies `brief`, `task`, `meta`, `submit`, `start`, `started`, `seal`, `payload` or `outcome`. Cleanup identifies its actual destination, so payload cleanup cannot satisfy an outcome checkpoint. Injected failure names are `fail-KIND-write`, `short-write`, `file-barrier`, `close`, `link`, `barrier`, `cleanup` or `cleanup-barrier`, with the common `fail-KIND-` prefix. Raw and ancestor hooks have their own destination-labelled operation names in the trace.

Admission boundaries additionally include `before-submit-consume`, `after-submit-consume` and `after-submit-sink`. Runner boundaries include `before-start-consume`, `after-start-consume` and `after-start-sink`; the sink boundary precedes `provider.started.json`. `raw-writer-open` is reached after stdout bytes are written while the writer remains owned. `before-seal` is reached at the seal link boundary after required raw barriers. Session operation boundaries are `before-session-claim` and `after-session-claim`, with corresponding release names. Record boundaries also accept KIND `session-claim-record` and `session-release-record`; these occur inside the session lease, so `before-session-claim-record-link` can hold a claimant after ownership examination and before publication.

The ordered `-events` log contains `<pid>:<operation>:<destination-basename>\n`. The separate `-fake-sink-event` log records actual `admission_sink` or `runner_sink` entries with the task ID. A guard alone is not a sink observation. Tests need a working recorder control and exact expected counts.

`lock-hold` supports bounded lock acquisition and cooperative release. Its close-on-exec child uses `-child-ready-file`, `-child-wait-file` and `-child-exit-file`; the finite child emits `<pid>:wait-exited-0\n` on its successful exit path. The competing process must acquire the lock while that child remains blocked and before the exit receipt exists. Parent process logs use regular files to avoid a child-held output pipe delaying observation of the parent's exit. No signal-driven timeout or forced child cleanup is part of acceptance.

## Exit observations

| Code | Fixture observation |
|---|---|
| 0 | Operation acknowledged successfully |
| 1 | Operational or fixture observation failure |
| 2 | Invalid arguments |
| 3 | Exact checkpoint self-exit, or unknown fake reconciliation |
| 4 | Candidate conflicts with a valid terminal winner |
| 5 | Invalid existing publication evidence/invariant fault |
| 10 | Submission intent already exists or admission lock is busy |
| 11 | Start intent already exists or runner lock is busy |
| 12 | Permit already consumed |
| 13 | Durability acknowledgment is uncertain |
| 14 | No valid seal for recovery |
| 15 | Invalid evidence or unavailable predicate |
| 16 | Collection blocked by a live runner |
| 20 | Continuation claim is busy or cannot be released |

These fixture codes are distinct from the future public `delegate` CLI contract. Tests assert the exact operation-specific result and preserve record bytes/inodes. Unit faults prove ordering and propagation; process exits prove kernel lock release. Neither is a physical power-loss experiment. Phase acceptance additionally requires independent review, clean gates and actual Linux/macOS CI on the final proposed source.
