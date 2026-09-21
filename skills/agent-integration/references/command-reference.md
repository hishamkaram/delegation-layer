# Command reference

All paths passed to `--root`, `--cwd`, and `--brief` must be absolute. Add
`--json` to commands that return an agent-consumable response.

| Command | Purpose | Launches provider work? |
| --- | --- | --- |
| `delegate --version` | Confirm the installed control CLI | No |
| `delegate providers --json` | List compiled provider metadata | No |
| `delegate capabilities --provider ID --json` | Read the catalog contract | No |
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
- `--permission read-only|workspace-write`: requested native permission mode (not independent containment);
- `--budget DURATION`: finite turn budget;
- `--auto`: let the CLI select a ready provider;
- `--provider ID`: use only when the caller explicitly selected an ID;
- `--model MODEL` and `--effort VALUE`: optional provider-advertised choices;
- `--native-timeout DURATION`: optional provider-native timeout where advertised.

Continuation flags:

- `--task TASK`: predecessor task ID;
- `--brief FILE`: optional new follow-up instruction;
- `--budget DURATION`, `--model MODEL`, and `--effort VALUE`: optional overrides.
