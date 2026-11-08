---
name: live-e2e
description: Verify affected CLI, provider and supervisor behavior using existing bounded drivers and honest platform claims.
---

# Live E2E

Use for a real CLI/provider/supervisor acceptance check. Read [provider checks](../../agents/_data/maintainability-contract.md#provider-checks)
and [lifetime ownership](../../agents/_data/delegation-invariants.md#3-supervision-deadlines-and-forbidden-signals).
For process/OS changes, also use [native checks](../../agents/_data/maintainability-contract.md#native-platform-checks).

## Inputs

Changed behavior and cases, actual binary/path under test, requested provider,
model/effort/permissions, finite budgets and available native host/authentication.
Use existing driver targets and [fixture installation options](../../docs/contributing.md#fixture-installations).
No new ledger, handoff manifest or hosted proof submission is required.
The [installed-executable rule](../../agents/_data/product-boundary.md#installed-executables)
applies to acceptance helpers and binary selection evidence as well as runtime.
Observe the selected installed path, invocation and required behavior; do not
claim immutable executable bytes from a version or path.

## Work

1. Build/use the actual changed CLI and runner. A supplied development pair
   proves its own scenario; a package/default-path claim must exercise that
   path. Do not silently replace the selected binaries or provider.
2. Choose affected success/failure controls. Native write modes need observable
   creation/edit/command results; native read-only behavior is provider-owned,
   not an independent Delegate sandbox. Use Git only where required.
3. Use [agent integration](../agent-integration/SKILL.md) for external work.
   Capture admission once; observe the same created task. Never redispatch for
   missing output, infer success from a queue row or bypass a Delegate blocker.
4. Verify the terminal outcome and requested result/artifacts. Committed
   publication alone does not prove assignment fulfillment. For continuation,
   verify the actual recorded native session and preserved predecessor.
5. Apply the project [cleanup rule](../../AGENTS.md#test-and-experiment-cleanup).
   Register exact owned resource handles and cleanup actions as they are
   created, using existing cleanup/finally/defer facilities. Delete owned test
   schedules to prevent further launches, then independently close admitted
   tasks. Stop only an owned host; a deleted definition or stop acknowledgement
   does not prove task closure. Verify canonical closure before releasing owned
   workspaces and removing temporary binaries, fixtures or profiles. Check the
   same cleanup after failed setup; report unresolved resources without deleting
   shared parents, retained history or user runtime settings.
6. Distinguish fixtures, native lifecycle, default installation and real provider
   results. Use existing driver outputs for diagnosis, including failed/bounded
   finalization. Missing auth/tool/host is blocked, not a passing skip.
7. Preserve actual model/effort and selected backend/assurance in the report.
   Private raw output and credentials stay out of public logs/artifacts. Do not
   add receipt/hash obligations unrelated to the changed behavior.

## Checks and result

Run the relevant existing scenario once on stable input. Do not run a full paid
matrix for unchanged runtime, repeat the aggregate after its subsets or install
another account/provider for a green result. Functional deadline and cleanup
bounds remain meaningful. Report command, binary/version/path, platform,
task/scenario outcome, cleanup and unavailable checks without claiming universal
proof. Root owns final integration and Git/publication and arranges independent review.

Report affected guidance and diagram changes under the project
[documentation completion rule](../../AGENTS.md#documentation-completion).
