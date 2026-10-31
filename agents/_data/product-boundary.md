# Product boundary

This brief is the normative product direction. Read it before proposing a design,
a guarantee, or a restriction. The active implementation plan tracks where the
current code still differs; this brief does not claim those differences are fixed.

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
- Provider CLIs change fast. Do not hash provider executables, keep private copies,
  build image pools, mirror installations, or gate on provider release versions.
  The immutability required of Delegate-owned records does not extend to external
  mutable tools.
- A reported version is an optional diagnostic, never a readiness condition.
- Accept additive, compatible CLI changes. Fail clearly when required behavior is
  actually missing, or when execution, authentication, protocol, or session
  handling really fails.
- Never silently change permissions or the destination, and never retry a task
  that may have started.
