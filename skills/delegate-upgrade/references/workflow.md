# Delegate upgrade workflow

## Establish the subject

Inspect `command -v delegate`, `delegate --version`, and installed help before
using version-dependent options. Read supervisor and scheduler status in the
intended user/configuration context. Omit `--root` for ordinary work: the CLI
resolves its configured default. Preserve an explicitly supplied or already
recorded custom root exactly throughout. Never assemble or search other roots.

Retain the existing upgrade operation ID and read phase, reason, next action,
and errors together. Completed historical migration is not current readiness.
Capture producer identities and whether each service or schedule is currently
running or paused. Do not print credentials, environments or provider output.

An upgrade request covers necessary preparation and completion. Diagnosis alone
does not authorize installation, cancellation or settlement. Reuse existing
user authorization; ask only for an unresolved user decision or an actual
required confirmation. Human settlement confirmation must remain explicit.

## Planned installation replacement

Run the handoff from a persistent user context outside Delegate-owned task
lifetime. Never dispatch a task to upgrade its own supervisor. If invoked from
such a task, report the outside-context command for the user instead.

Identify the installed package before preparation or installer changes. For Homebrew,
inspect `brew list --versions` and match the selected Delegate installation to
its installed formula. Use that exact formula for `brew upgrade`; the executable
name `delegate` does not establish a formula name. If native inventory cannot
establish the package, stop with the preserved OP instead of guessing.

Quiesce authorized scheduler, connector and external producers through their
supported controls, including other-root aliases to the same private endpoint.
Drain fences participating admissions only. Preserve definitions, service
profiles and credentials.
Do not enable producers or schedules that were already stopped or paused.

Check `delegate help supervisor ensure-owner`. Where supported, run
`delegate supervisor ensure-owner --prepare-upgrade --json` before replacing
the installation. Keep the returned OP and completion command. Exit 3 means
pending: inspect the exact blocker and observe/resume that same operation.
Only `awaiting_installation` with preparation complete permits replacement.
Older builds without preparation support use their documented reactive path.

Update through the user's selected installation mechanism. Never silently
switch Homebrew to a shell/npm installation, retain an old keg as recovery,
change provider setup, or overwrite an unrelated installation. Verify the new
CLI selection, then execute the returned completion argv with that installation.
A proactive operation requires explicit completion, including same-path updates.

## Installation already replaced or operation interrupted

Inspect the existing OP before another mutation. Ordinary
`delegate supervisor ensure-owner --json` resumes a reactive handoff when the
selected installed pair changed. Do not create a replacement OP, reinstall
again, or repeatedly refresh the owner to escape a blocker.

Finish this handoff before another installation replacement. Before target
acceptance, the same operation may reselect a missing candidate using the current
compatible installed pair. After acceptance the target stays pinned. An unavailable
accepted target is a fault; retain the OP and report its exact guidance.
A lost acknowledgement is not permission to replay a task or mutation.

A retained drain may recover its stopped predecessor once. It requires the
exact saved installed pair, verified stopped identity and unchanged restore
source, settled current/historical daemon Wait custody, and authenticated owner
and bootstrap exclusion. Offline queue checks only reject unsafe recovery;
advancement requires the actual live queue and canonical maintenance proof.

The retained drain owns Start acceptance and birth. Unknown birth, changed or
unknown boot, closed Wait, or absent/mismatched recorded child requires
`stop_and_report`, including with exit 3. A later healthy endpoint cannot clear
it. Only the live invocation proving Start never occurred may roll acceptance
back; there is no automatic reset or provider retry. Older strict readers refuse
a populated predecessor Start receipt. Keep them off that root and retain the
OP when recovery is exhausted.

## Follow the actual blocker

Use the installed build's typed reason and next action. An admission-only
ordinary task may require explicit `cancel`; do not cancel unrelated work or
infer task closure from a missing queue row or executable. Inspect the exact
ID and request before taking a user-authorized cancellation action.

Complete historical inspections may qualify from their validated journal
records. Expired unstarted inspections use their supported recovery owner.
Wait only for an actual owned operation or unexpired deadline. Distinguish
pending handoff from storage, identity, installation and cleanup faults.

For a supported unresolved inspection, use the installed settlement command
and explicit inspection kind. Prepare is observational. Present the challenge,
permission, retained claims and unresolved native facts to the human. Never
confirm a real challenge automatically. Settlement grants supervisor maintenance
only; it does not publish a result, prove native closure, or release resource,
session, workspace, custody or deletion claims. A positive live identity or
unprovable legacy execution cannot be waived by confirmation.

No moving/deleting records, direct process killing, manual Pueue repair,
executable copying/fingerprinting, invented roots or silent replay is a recovery
step. A concrete refusal ends that attempt; bounded read-only diagnosis may
continue. Preserve actionable evidence rather than retrying uncertain work.

## Explicit Gateway root context repair

For a saved SDK root token that no longer matches after a device-number change,
use the installed `gateway repair-context` command only for the same original
root, canonical path and inode. Do not re-enroll or edit the profile, credentials
or task history. No automatic stable Darwin volume-identity mapping is used.

1. Select/install the reviewed compatible build. Run
   `delegate supervisor ensure-owner --refresh-owner --json` when an old helper
   needs replacement, then finish the same retained OP. Reactive mode uses
   ordinary ensure; proactive mode needs the saved `--complete-upgrade OP`
   command. Require completed handoff and no pending generation transition.
2. Stop the Gateway connector, scheduler and other writers/frontends through
   their supported controls. Keep the compatible daemon running and ready for
   confirmation; its healthy current helper may remain running.
3. Run `delegate gateway repair-context --json`. Prepare is passive and returns
   digests, origin/accepted coordinates, request counts and a concrete challenge,
   never the saved token, its wire guard or secrets. On the first repair it
   checks validated startup history; later repairs use the immutable binding
   origin. Every canonical task/inspection request is checked. A matching legacy
   context reports `root_context_not_required` with exit `1` and
   `run_gateway_doctor` without enabling the feature.
4. Obtain the human's explicit confirmation of that exact challenge and offline
   assertion: intended original root, no other live copy, incompatible
   writers/frontends stopped. Never auto-confirm. Then run
   `delegate gateway repair-context --confirm CHALLENGE --offline-cutover-confirmed --json`.
5. Run `delegate gateway doctor --json`; only after the root match is verified,
   restore the saved service with `delegate gateway service start --json` if it
   was running, and restore other producers to their original state.

Use the same optional `--profile ABS` throughout Gateway commands. Omit `--root`
for the CLI default; preserve the exact explicitly selected or recorded
custom/historical `--root ABS` otherwise. Never invent or search another root.
Confirmation revalidates under profile/identity, bootstrap, live root-wide
quiescence and nonblocking maintenance exclusions. It atomically adds `context_binding` and required
`root-context-binding-v1` to `root.json`; registration, profile, credentials,
canonical requests and outcomes retain their bytes. Older strict readers refuse
the new feature on their next open; already-open incompatible writers still
require operator closure. The original token preimage stays immutable; every
acquisition must match the accepted canonical path/device/inode exactly. Another
device-number change requires another explicit repair. Moved,
replaced or copied roots refuse; the no-other-live-copy fact remains the
operator's attestation. Local checks cannot prove closure of nonparticipating old writers or absence
of another copy. A refusal is not permission to reset state or retry work.

Read the typed failure and next action with the exit status. Exit `0` completes
only prepare/confirmation, not service or provider readiness:

| Failure | Exit | Required action |
| --- | --- | --- |
| `root_context_transition_pending` | `3` | `complete_existing_supervisor_transition`; finish the same OP. |
| `root_context_runtime_active` | `3` | `stop_gateway_service_and_producers`. |
| `root_context_native_work_pending`, including maintenance-lock contention | `3` | `wait_for_native_work`; do not bypass the owner. |
| `root_context_challenge_stale` | `1` | `prepare_root_context_reconciliation`; obtain human confirmation for that exact new challenge. |
| `root_context_history_unavailable` or `root_context_evidence_unreadable` | `1` | `stop_and_report`; evidence, observation and cleanup faults are not retryable busy. |
| `native_context_changed` | `1` | `stop_and_report`; never replace a profile or try another origin to hide a binding/token mismatch. |

Pure cancellation or deadline reports `gateway_operation_interrupted`, exit `1`,
`inspect_same_profile`. An interrupted confirmation may already have committed;
do not infer rollback or confirm again automatically. Run passive
`gateway repair-context` on that same profile, or `gateway doctor`, first.
`already_reconciled: true` means the binding is committed: run doctor rather than
confirm again. A joined concrete fault remains an evidence failure. No response
authorizes a task/provider retry or state reset.

## Verify and restore

Resume the same handoff until the response proves current supervisor readiness;
exit zero alone can mean preparation, helper refresh or historical completion.
Restart only producers this workflow stopped, from the verified installation,
and restore their original state after supervisor readiness. Check service
selection and scheduler readiness before resuming an explicitly requested
schedule. Preserve previously paused schedules unless the user requests resume.

Report installation selection, supervisor completion/readiness, scheduler
readiness and any actual scheduled occurrence separately. An installed binary
or ready scheduler does not prove provider execution succeeded. Report exact
OP, commands/results, unresolved blockers and any producer still stopped.
