# Pre-change scheduler state

Frozen serialized fixture produced from commit
`bfc308cdb74dfb27d7be61d0343e929231a91dcb`, before the history/execution changes.
The unchanged version-1 SQLite database and task records were copied after the
old database connection closed. Do not regenerate with current writers.

The one-off producer used that checkout's `newAppTestTask`,
`newAppTaskInStore`, `recordCustodyLifetime`, `openScheduleForCreate` and the
production claim/observation/release APIs. Both tasks contain the fixture
answer `answer\n`. Schedule ID is 32 `a` characters. Task IDs are 32 `c`
characters (sealed, released, publication pending) and 32 `d` characters
(released, publication committed). Release references bind the old validated
termination records.

All data is synthetic: inert `/tmp/app-*` executable/supervisor paths and
controlled lifetime-record fixtures. Nothing was launched, and the lifetime
records do not prove native OS behavior. There are no accounts, credentials or
private provider output. Temporary workspace paths are historical metadata;
they need not exist to inspect these tasks.

Tests materialize private permissions before using current production readers
and collection. They verify unchanged history, truthful pending/committed
evidence and same-task recovery without rerunning a provider or changing the
old occurrence/release. This fixture proves persisted-format compatibility;
the separate native acceptance proves real scheduling and process behavior.
