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
| `delegate continue --task TASK ... --json` | Create an exact-session successor | Yes, at most once for the successor |
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
- `--provider ID`: use only when the caller explicitly selected an ID;
- `--model MODEL` and `--effort VALUE`: optional provider-advertised choices;
  Codex maps effort to TOML `model_reasoning_effort`, while Claude and AGY
  pass native effort flags; Pi maps effort to `--thinking`; OpenCode maps it to
  `--variant`; `default` is omitted;
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
- `--budget DURATION`, `--model MODEL`, and `--effort VALUE`: optional overrides.
