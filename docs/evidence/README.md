# Evidence

Reproduction harnesses for claims in [`../IMPLEMENTATION-PLAN.md`](../IMPLEMENTATION-PLAN.md).
Saved as `.txt` so they are not compiled by the module.

## `publication_primitive_*.go.txt`

Establishes the publication protocol in *Measured during planning*. Run in a scratch module:

```bash
mkdir /tmp/pubtest && cd /tmp/pubtest && go mod init pubtest
cp .../publication_primitive_main.go.txt main.go
cp .../publication_primitive_test.go.txt split_test.go
go run .                       # link is atomic create-if-absent
go test -race -v               # split-brain, and the single-outcome fix
```

Results on macOS 26.6.2 / APFS / Go 1.27.1, 2026-09-13:

| Claim | Result |
|---|---|
| `os.Link` is atomic create-if-absent — 8 concurrent publishers, unique staging files | `winners=1` in **3/3 runs**; every loser `EEXIST` |
| A late writer cannot overwrite a committed record | `link ...: file exists` in **3/3 runs** |
| `link` alone does **not** prevent two *different* terminal pathnames both committing | both records present in **200/200 runs** |
| A single `outcome.json` claim resolves it | ambiguous or missing verdict in **0/200 runs** |

The third row is the one that matters: it refutes the first version of this plan, which claimed
`link` alone made a result-and-rejection pair impossible.

**Not established here:** crash durability, behaviour on any filesystem other than APFS, or
behaviour under `fsync` failure. The plan treats those as Phase 1 exit work.
