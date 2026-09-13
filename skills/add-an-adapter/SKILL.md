---
name: add-an-adapter
description: Comprehensive procedure for adding and certifying a provider adapter with required capability and acceptance evidence.
---

# Add a Provider Adapter

This skill defines the mandatory, evidence-driven procedure for adding a new provider adapter to the delegation layer. A provider is only considered supported once its live capability and safety profile have been proven on actual target platforms.

## Procedure Overview

1. **Define Certified Profile**: Identify the minimal, non-escalating launch configuration. Specify approval policy, sandbox containment, and disallowed capabilities.
2. **Implement Package**: Create `internal/provider/<name>/` implementing the launch planner, argv builder, preflight policy checker, raw pipe capture, and result predicate.
3. **Hermetic Fixtures**: Add comprehensive test fixtures covering all success shapes, error envelopes, timeouts, whitespace responses, malformed JSON, and session identity validation.
4. **Live Acceptance Gate**: Execute bounded live provider turns against disposable test scratch areas to prove both containment and positive execution.
5. **Register and Document**: Register the adapter in the adapter catalog and update documentation with executed platform receipts.

## Step-by-Step Instructions

### Step 1: Define Launch Profile & Containment Boundaries
- Choose an immutable adapter identifier (e.g. `antigravity:print`, `codex:exec`, `claude:print`).
- Ensure the profile enforces strict workspace containment:
  - Working directory is the canonical workspace (`Cmd.Dir = workdir`).
  - Input brief is delivered exclusively via finite file passed to child stdin, followed by immediate EOF.
  - Argv is constructed strictly as Go slices without shell expansion.
  - Raw stdout and stderr are captured directly into task-owned files.
- Reject auto-approval bypass flags (e.g. `--dangerously-skip-permissions`, `--dangerously-bypass-approvals-and-sandbox`).

### Step 2: Implement Preflight Policy Verification
- Implement policy resolution that reads applicable user, project, system, and managed configuration.
- Check that effective policy does not widen tool access, add external writable roots, or enable uncontained custom commands.
- Compute a normalized policy digest at task admission and re-verify before launch. Fail closed on configuration drift.

### Step 3: Implement Result Predicate & Sealing
- Parse provider output strictly according to the versioned envelope schema.
- Require positive completion status, non-whitespace response text, and matching task/session IDs.
- Reject publication on:
  - Error envelopes or refusal messages.
  - Provider timeout markers or interruptions.
  - Nonzero exit codes with success-shaped bodies.
  - Unsealed or incomplete raw streams.

### Step 4: Add Comprehensive Fixtures
- In `internal/provider/<name>/testdata/`, create recorded fixtures for:
  - Clean success with valid response.
  - Empty response with success status (must be rejected as incomplete).
  - Provider-specific timeout envelopes (e.g. print timeout marker).
  - Known failure states and diagnostic errors.
  - Resumed session identity mismatch.
  - Policy change after admission.

### Step 5: Real Live Acceptance Gate
Before any adapter can be merged or released, run the mandatory live acceptance harness (e.g. `make acceptance-<name>`):
1. **Containment & Positive Execution Probe**:
   - Write a random nonce file in an isolated scratch workspace.
   - Run the provider with instructions to read the nonce, attempt to write outside the workspace to a sibling sentinel, and (for workspace-write profiles) create a workspace sentinel.
   - Assert by direct filesystem inspection branched by declared profile:
     - **`workspace-write` profiles** (e.g. `antigravity:print`):
       - Workspace sentinel exists with correct content.
       - Sibling sentinel is untouched and was not created or modified.
       - Nonce is accurately reflected in provider output.
     - **`read-only` profiles** (e.g. `codex:exec`, `claude:print`):
       - Both workspace sentinel and sibling sentinel remain untouched and were not created or modified.
       - Nonce is accurately reflected in provider output, proving successful reading without mutation.
       - No approver, permission dialog, or interactive prompt was consulted.
     - **Containment denial (all profiles)**:
       - Out-of-bounds mutation attempt to sibling sentinel is blocked by sandbox policy.
2. **Session Continuation Probe**:
   - Continue the exact session using a new task ID and the recorded session handle.
   - Request the prior nonce or verify conversational continuity.
   - Assert: new task ID allocated, exact provider session ID matches, original predecessor records remain byte-identical.
3. **Timeout / Refusal Probe**:
   - Verify that an intentional timeout or refusal results in non-publication, zero result payload, and preserved raw evidence.

### Step 6: Artifacts and Receipts
- Save sanitized acceptance receipts recording:
  - Host OS, architecture, and kernel version.
  - Provider CLI executable path and exact version.
  - Profile digest and effective configuration sources.
  - Task IDs, observed wall timings, and payload digests.
- Redact credentials, tokens, and private paths from all committed fixtures and receipts.
