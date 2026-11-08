# Product boundary

This brief owns product direction. Read it before proposing a design,
guarantee or restriction. Implemented behavior belongs in the current guides.

## What Delegate is

Delegate manages the lifecycle of one bounded task run by an agent CLI that
the user installed. Tasks may run locally or on a prepared remote destination;
the executing installation owns admission, supervision and terminal evidence.
The origin's remote reference records intent and observations, not a second task
outcome. Optional Teams provide reusable task-selection configuration without
adding general workflow orchestration.

## Delegate owns

- durable task metadata, at-most-once launch, budgets, process supervision and
  canonical closure, and cancellation;
- exact-session successor bookkeeping for explicit continuation;
- optional managed workspace capture;
- sealed result and artifact evidence; and
- its own installation, worker, bundled private supervisor, and their identities,
  including verification of downloaded release/install archives, subject to the
  installed-executable rule below.

## The provider and the user own

CLI installations and updates, accounts and authentication, settings, extensions,
and native session storage. Delegate does not manage, audit, sandbox, or police
them, and it adds no independent provider policy engine.

## Installed executables

This rule applies throughout the project: Delegate, delegate-run, Pueue,
provider CLIs, callbacks, helpers, tests, fixtures and contributor/release tools.

- Execute the selected installed files in place. Do not copy, stage, mirror or
  content-address installed executables into state roots, runtime caches,
  workspaces or proof fixtures. A prior implementation is not an exception.
- Do not calculate or require executable-content SHA/checksums/fingerprints,
  including for diagnostics, acceptance receipts, build identity or recovery.
  Do not disguise an executable digest as a version, record or compatibility ID.
- The installation resolver owns selection; launch and recovery read that
  selection. Validate the required native behavior, protocol/capabilities and
  process/root/endpoint identity. A version or path alone does not prove byte
  immutability. An incompatible or missing installation fails explicitly;
  uncertain work is never replayed.
- Ordinary installation/extraction of a verified distribution archive and
  building an owned candidate installation remain supported. Permitted download
  integrity covers archives and source inputs. A raw downloaded executable is
  still an executable: its content hash is prohibited, including before first
  use or packaging. Replace such download paths with a verified archive/source
  route before use; never obtain compliance by shipping unverified binaries.
  Availability of that route must be established, not assumed.
- Preserve task/result/artifact evidence, configuration and persisted-record
  integrity digests, source-input evidence and diagram checksums. Classify by
  the bytes being hashed, not a field or module name. Historical executable
  digest fields may be decoded as historical data; they do not authorize new
  executable hashing or rewriting sealed evidence.

## Rules for external provider CLIs

- Preserve the requested provider, model, effort, native permission mode, and
  workspace exactly. Discovery is advisory.
- Delegate identifies the installed provider executable by path metadata only
  and does not read its contents. Required native CLI behavior determines
  compatibility. Provider release numbers are optional diagnostics.
- Launch the installed tool in place with its native configuration and sessions.
  Provider installations remain external to Delegate's managed state and
  user/provider-owned. Delegate owns its own installation, supervisor identities,
  task state and evidence. Documented native configuration/session reads needed
  for permissions or continuation remain part of the adapter contract.
- Accept additive, compatible CLI changes. Fail clearly when required behavior is
  actually missing, or when execution, authentication, protocol, or session
  handling really fails.
- Never silently change permissions or the destination, and never retry a task
  that may have started.
