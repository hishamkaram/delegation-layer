# Command reference

All paths passed to `--root`, `--cwd`, and `--brief` must be absolute. Add
`--json` to commands that return an agent-consumable response.

| Command | Purpose | Launches provider work? |
| --- | --- | --- |
| `delegate --version` | Confirm the installed control CLI | No |
| `delegate providers --json` | List compiled provider metadata | No |
| `delegate capabilities --provider ID --json` | Read the catalog contract | No |
| `delegate models --provider ID [--cwd ABS] --json` | Discover advisory native model and effort facts | Native inspection only; no admitted task |
| `delegate preflight --provider ID --cwd ABS --json` | Check static provider admission | No |
| `delegate dispatch --auto ... --json` | Select and admit one bounded turn | Yes, at most once |
| `delegate status TASK --json` | Reconcile and observe task state | No provider launch |
| `delegate collect TASK --watch 5s --json` | Read or wait for publication | No provider launch |
| `delegate recheck TASK --json` | Reassess eligible rejected evidence offline | No provider launch; original outcome unchanged |
| `delegate continue --task TASK ... --json` | Create an exact-session successor | Yes, at most once for the successor |
| `delegate reply TASK --request ID --answers FILE --id MESSAGE_ID --json` | Answer a pending native question | No new launch |
| `delegate steer TASK --brief FILE --id MESSAGE_ID --json` | Guide a supported active turn | No new launch |
| `delegate events TASK --after SEQUENCE --watch 5s --json` | Read a bounded interaction event page | No |
| `delegate logs TASK --json` | Return validated output descriptors | No |
| `delegate cancel TASK --json` | Record one explicit stop request | Stops, does not relaunch |

The bundled supervisor is managed by the released CLI under the state root.
Collection remains observational. Provider authentication remains the
provider’s own responsibility and is never performed by this skill.

Dispatch flags:

- `--brief FILE`: bounded task instructions;
- `--cwd ABS`: workspace for the delegated turn;
- `--permission read-only|workspace-write`: read-only investigation or authorized unattended write execution, including commands (not independent containment);
- `--budget DURATION`: finite turn budget;
- `--auto`: let the CLI select a ready provider;
- `--provider ID`: preserve the caller’s named harness or provider, resolved through the catalog;
- `--model MODEL` and `--effort VALUE`: provider-advertised choices (CCR requires a model alias);
  Codex maps effort to TOML `model_reasoning_effort` for ordinary turns or a native
  turn parameter for interactive turns, while Claude and AGY
  pass native effort flags; Pi maps effort to `--thinking`; OpenCode maps it to
  `--variant`; CCR forwards effort to Claude and uses `--model` as a CCR alias; `default` is omitted;
- `--interactive`: enable advertised native clarification and steering; see [interaction](interaction.md);
- `--native-timeout DURATION`: optional provider-native timeout where advertised.

Model discovery uses the same `--provider ID` profiles and accepts an optional
absolute `--cwd` (default: current directory). Its JSON has `status` values
`available`, `partial`, `blocked`, `unavailable`, or `failed`; exits are `0`,
`0`, `2`, `2`, and `1` respectively. `complete` describes model enumeration.
Top-level or per-model `efforts: null` means unknown, while `[]` means no
choices were explicitly reported. Discovery is advisory and never an admission
allowlist; defaults remain valid when it is not run.

Continuation flags:

- `--task TASK`: predecessor task ID;
- `--brief FILE`: optional new follow-up instruction;
- `--budget DURATION`, `--model MODEL`, and `--effort VALUE`: optional overrides;
- `--interactive`: enable interaction when supported; already-interactive predecessors keep it.

## Choosing models and effort

Pass an explicitly supplied model ID and effort unchanged, even if discovery
is incomplete or does not list them. To resolve a natural-language model name
or choose an option the caller left open, use native discovery:

```sh
delegate models --provider PROVIDER_ID --cwd ABSOLUTE_WORKSPACE --json
```

Omit `--cwd` to use the current directory. Run discovery only when the caller
needs a non-default choice; provider defaults are valid. The command uses the
state-rooted supervisor and a bounded `model-discovery-v1` five-minute native
inspection to allow first-run provider model/cache initialization. It creates
inspection evidence without admitting a provider task, and may contact the
provider CLI. It is advisory and never an admission allowlist. Do not reject a
requested value merely because discovery did not list it, and do not infer
per-model effort support from the harness-wide list. If a selected model has
`efforts: null` or `efforts: []`, omit `--effort` unless the caller explicitly
requested it; do not invent model aliases or combine options by guessing.
If a natural-language name cannot be resolved to a single exact ID, ask one
focused question for that ID. Do not silently select a default or transform
spaces into hyphens to manufacture an ID. This ambiguity rule is guidance for
selecting a value, not a new admission restriction.

The response includes `schema_version`, `command: "models"`, `provider`,
`observed_at`, `status`, `reason_code`, `complete`, `source`, nullable
`efforts`, and model rows with exact `id` values plus optional `name`, nullable
`efforts`, and optional `default_effort`. `complete` describes model
enumeration, not effort metadata. `null` means effort metadata is unknown;
`[]` means the provider explicitly reported no choices. Discovery exits `0`
for `available` or `partial`, `2` for `blocked` or `unavailable`, and `1` for
`failed`. Preserve a blocked or unavailable discovery result as unresolved;
defaults remain allowed.

## Checking readiness

Use `preflight` when the caller asks about static readiness, rather than as a
required step before every dispatch:

```sh
delegate preflight --provider PROVIDER_ID --cwd ABSOLUTE_WORKSPACE \
  --permission REQUESTED_MODE --json
```

Use `read-only` for investigation or `workspace-write` for authorized edits.
Supply any required provider options, including `--model ALIAS` for `ccr:print`.
Preflight does not create a task, start the supervisor, or launch a provider.
Its result is not proof of live authentication or successful execution. Report
`blocked` or `unsupported` results; a readiness request does not authorize
provider configuration changes. Honor `failure.next_action` without probing
provider CLIs yourself.
