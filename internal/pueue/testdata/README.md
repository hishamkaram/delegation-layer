# Native status fixture

`status-4.0.4-queued.json` is a sanitized actual pueue 4.0.4 `status --json` response on macOS arm64. It retains numeric identity, timestamps, status, field names, groups, and environment key names. Commands, workspace, label and all environment values are replaced with inert fixture values.

Original response SHA-256: `156af49975644189c19f2ae2582dd3877a65fd2244fdceb76600116d460185d1`. The originating read-only status command exited 0. Its enclosing reconnaissance run later failed an environment assertion; this fixture proves only the observed queued response shape, not successful lifecycle completion.

Schema source: [pueue 4.0.4 task values](https://github.com/Nukesor/pueue/blob/v4.0.4/pueue_lib/src/task.rs).
