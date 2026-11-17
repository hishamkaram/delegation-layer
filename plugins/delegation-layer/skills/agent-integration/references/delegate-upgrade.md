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
supported controls. Preserve definitions, service profiles and credentials.
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

Before target acceptance the existing operation may select the compatible
installed pair. After acceptance the target stays pinned. An unavailable
accepted target is a fault; retain the OP and report its exact guidance.
A lost acknowledgement is not permission to replay a task or mutation.

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
