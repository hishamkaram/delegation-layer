# JSON decision sketch

```sh
delegate --root "$STATE_ROOT" status "$TASK_ID" --json
delegate --root "$STATE_ROOT" collect "$TASK_ID" --watch 5s --json
```

Decision order:

1. Save `task_id`.
2. Check `admission` and `error`.
3. Wait while publication is pending.
4. Accept only a committed terminal `outcome`.
5. If `status` is `timed_out`, follow `continuation.resumable`.
