# Product boundary

This brief owns product direction. Read it before proposing a design,
guarantee or restriction. Implemented behavior belongs in the current guides.

## What Delegate is

Delegate manages the local lifecycle of one bounded task run by an agent CLI that
the user installed. It is not a team or general workflow product.

## Delegate owns

- durable task metadata, at-most-once launch, budgets, process supervision and
  canonical closure, and cancellation;
- exact-session successor bookkeeping for explicit continuation;
- optional managed workspace capture;
- sealed result and artifact evidence; and
- its own installation, worker, bundled private supervisor, and their identities,
  including verification of its own release and install artifacts.

## The provider and the user own

CLI installations and updates, accounts and authentication, settings, extensions,
and native session storage. Delegate does not manage, audit, sandbox, or police
them, and it adds no independent provider policy engine.

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
