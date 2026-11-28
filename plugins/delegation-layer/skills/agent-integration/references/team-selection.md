# Automatic Team discovery and Member selection

The lead responsible for the user's request discovers Teams before deciding
whether to keep or assign substantive research, planning, implementation,
testing or review work, including work it expects to keep local. Skip automatic
discovery for quick answers, tool/configuration discussion, explicit native-only
or no-delegation instructions, and named providers, models or integrations;
still resolve any explicit Team/Member target. Agents working from bounded
briefs or assignments skip discovery unless explicitly authorized to allocate
work. A Team grants no permission, budget or authority to invent assignments.

1. Keep the actual absolute workspace `--cwd` throughout discovery. Use an
   explicit `--project-root` only for an already identified non-Git project.
   Read `teams default show` and `teams list` in that same context. The default
   response already contains its full Team record; do not fetch it again.
   List includes Global and current-project Teams; never infer Local identity
   from a remote URL or silently prefer a same-named Local Team.
   Retain the selected Team's complete inspected definition and revision for
   this decision, including optional `member_unavailable`. Its application is
   owned by [unavailable Team members](result-handling.md#unavailable-team-members).
2. Follow returned `next_cursor` values unchanged with bounded list pages.
   Inspect full definitions of plausible alternatives before selecting Members,
   using each list row's `reference.scope` and `reference.team_id`. User scope uses
   `teams show --scope user --team TEAM_ID --json` without project context.
   Project scope uses `teams show --scope project --team TEAM_ID --cwd ABS --json`,
   retaining step 1's explicit `--project-root` when applicable. A generic or
   truncated summary cannot rule out an uncertain candidate. Look for alternatives
   even when the default is suitable.
3. Match the assigned work to Members' Roles and supported provider capabilities.
   Prefer the default Team among comparably suitable choices. Another Team is
   clearly better when its Role fits a required specialty the default lacks, or
   the default lacks the required provider capability. If fully equivalent
   choices remain, use the default first, then stable scoped Team/Member ID order.
   Ask when unresolved choices materially change Role, provider, model, effort
   or native write behavior. Do not choose the first Member merely by definition
   order. State the selected scope, Team, Member and reason before dispatch or
   schedule creation.
4. Prefer a fitting Member when assigning a phase to a separate worker, including
   implementation. The lead may keep work itself; finding a Team does not require
   fan-out. An explicit Team/Member is binding; if only a Team was named, choose
   within it. A named provider or model without a Team stays direct with no
   automatic Team flags, preserving omitted native model/effort defaults.
5. Use the explicit target flags in [Using a Member](command-reference.md#using-a-member) and only
   the caller's original overrides. Do not promote inspected Member defaults
   into explicit provider/model/effort flags. Admission resolves current defaults
   and owns the immutable task context. Report admitted revision/settings and
   material drift from inspection; do not cancel, replay or replace work for drift.

Refresh advisory reads at each new substantive work decision or batch. Reuse them
within that decision only; no session cache is authoritative. Paginated reads
are not one atomic catalog snapshot, so describe an explainable fit from the
observed roster rather than a globally unique or permanently best choice.

`partial` status, truncated diagnostics, a page error, changed `catalog_id`, or
changed/omitted project context makes discovery incomplete. If the available
control/context budget prevents finishing pagination, say so; never report an
exhaustive comparison, absence or unique fit from an incomplete view. A known
valid default or other inspected Member may still be used when the assignment
is clear, with that limitation disclosed; ask if it affects the choice. A Team
deleted between list and show is an unavailable candidate, not an empty catalog.

Missing catalog or no suitable Member leaves ordinary authorized work available.
Report stale defaults and storage/project diagnostics accurately. Optional read
failures do not block an explicit direct-provider request. A missing explicitly
requested or selected Team/Member blocks that attempt: do not silently substitute
or retry it. Discovery never authenticates, probes readiness, initializes or
repairs storage, changes preferences, or launches a task. Observation, replay
and exact-session continuation retain saved selection without discovery.
