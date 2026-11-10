package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func require(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type sourceFixture struct {
	store   *taskdir.Store
	options Options
	cwd     string
}

func newSource(t testing.TB) sourceFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	store, err := taskdir.InitStore(root)
	require(t, err)
	t.Cleanup(func() { require(t, store.Close()) })
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	home, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	return sourceFixture{store, Options{Root: store.Root, Environment: []string{"HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, "cache")}, Registry: predicate.Default()}, cwd}
}

func (f sourceFixture) task(t testing.TB, index int, at string) string {
	t.Helper()
	id := fmt.Sprintf("%032x", index)
	brief := []byte("private-brief-marker-do-not-index")
	configuration := task.TaskConfig{Permission: "read-only", Budget: "1m0s", Model: "requested-model"}
	request := &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: f.store.RootID, TaskID: id, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: f.cwd,
		BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)), RequestedConfig: configuration,
	}
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: f.store.RootID, TaskID: id, Predicate: task.FixturePredicateRef(), RequestedConfig: configuration,
		EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("effective"))}, Containment: "fixture-only", Approval: "never",
		ProviderExecutable: filepath.Join(f.cwd, "provider"), ProviderVersion: "fixture-v1", PublisherBuild: "private-build-marker-do-not-index", PublisherVersion: "test",
		SupervisorConfig: task.SupervisorRef{ConfigPath: filepath.Join(f.cwd, "pueue.yml"), ConfigDigest: task.ComputeSHA256([]byte("pueue")), Endpoint: filepath.Join(f.cwd, "socket"), ObservedVersion: "fixture-v1"}, CreatedAt: at,
	}
	td, err := f.store.CreateTask(id, request, brief, meta)
	require(t, err)
	require(t, td.Close())
	return id
}

func openCatalog(t testing.TB, options Options) *Catalog {
	t.Helper()
	catalog, err := Open(context.Background(), options)
	require(t, err)
	t.Cleanup(func() { require(t, catalog.Close()) })
	return catalog
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	values := map[string]string{}
	require(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			values[path] = string(data)
		}
		return nil
	}))
	return values
}

func TestCatalogGlobalOrderingPagingSearchAndSourceIsReadOnly(t *testing.T) {
	f := newSource(t)
	old := f.task(t, 3, "2026-01-01T00:00:00Z")
	newer := f.task(t, 1, "2026-03-01T00:00:00Z")
	partial := f.task(t, 2, "2026-02-01T00:00:00Z")
	require(t, os.Remove(filepath.Join(f.store.Root, "tasks", partial, "meta.json")))
	before := snapshot(t, f.store.Root)
	c := openCatalog(t, f.options)
	progress, err := c.Scan(t.Context())
	require(t, err)
	if progress.Indexed != 3 || progress.Incomplete || progress.Unobserved != 3 {
		t.Fatalf("wrong scan coverage: %+v", progress)
	}
	var ids []string
	query := Query{Limit: 1}
	for {
		page, queryErr := c.Query(t.Context(), query)
		require(t, queryErr)
		if len(page.Rows) != 1 {
			t.Fatalf("unexpected page size: %d", len(page.Rows))
		}
		ids = append(ids, page.Rows[0].TaskID)
		if page.Next == "" {
			break
		}
		query.After = page.Next
	}
	if !reflect.DeepEqual(ids, []string{newer, old, partial}) {
		t.Fatalf("creation ordering used IDs or invented partial date: %v", ids)
	}
	page, err := c.Query(t.Context(), Query{Search: "REQUESTED-MODEL"})
	require(t, err)
	if len(page.Rows) != 3 {
		t.Fatalf("global metadata search omitted rows: %d", len(page.Rows))
	}
	assertPrivateMetadataAbsent(t, c)
	_, err = c.RefreshObservations(t.Context(), 2)
	require(t, err)
	after := snapshot(t, f.store.Root)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("passive history changed execution-root bytes or files")
	}
}

func assertPrivateMetadataAbsent(t *testing.T, c *Catalog) {
	t.Helper()
	var page Page
	var err error
	for _, marker := range []string{"private-brief-marker-do-not-index", "private-build-marker-do-not-index"} {
		page, err = c.Query(t.Context(), Query{Search: marker})
		require(t, err)
		if len(page.Rows) != 0 {
			t.Fatalf("indexed private content %s", marker)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			data, readErr := os.ReadFile(c.CachePath() + suffix)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if bytes.Contains(data, []byte(marker)) {
				t.Fatalf("private content copied into cache%s", suffix)
			}
		}
	}
}

func TestCatalogSingleScannerTakeoverAndCoordinatedRebuild(t *testing.T) {
	f := newSource(t)
	f.task(t, 1, "2026-01-01T00:00:00Z")
	first := openCatalog(t, f.options)
	_, err := first.Scan(t.Context())
	require(t, err)
	second := openCatalog(t, f.options)
	if _, err = second.Scan(t.Context()); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("second scanner acquired writer role: %v", err)
	}
	page, err := second.Query(t.Context(), Query{})
	require(t, err)
	if len(page.Rows) != 1 {
		t.Fatal("non-writer could not read committed pages")
	}
	reset := f.options
	reset.Rebuild = true
	if _, err = Open(t.Context(), reset); !errors.Is(err, ErrCacheBusy) {
		t.Fatalf("rebuild replaced another session's open database: %v", err)
	}
	require(t, first.Close())
	_, err = second.Scan(t.Context())
	require(t, err)
	require(t, second.Close())
	rebuilt := openCatalog(t, reset)
	if !rebuilt.Rebuilt() {
		t.Fatal("explicit rebuild was not reported")
	}
	page, err = rebuilt.Query(t.Context(), Query{})
	require(t, err)
	if len(page.Rows) != 0 || !page.Progress.Incomplete {
		t.Fatal("rebuild pretended derived rows survived")
	}
}

func TestCatalogChangedMetadataIsFaultAndFailedScanCannotSweep(t *testing.T) {
	f := newSource(t)
	firstID := f.task(t, 1, "2026-01-01T00:00:00Z")
	secondID := f.task(t, 2, "2026-01-02T00:00:00Z")
	c := openCatalog(t, f.options)
	_, err := c.Scan(t.Context())
	require(t, err)
	require(t, os.RemoveAll(filepath.Join(f.store.Root, "tasks", firstID)))
	metaPath := filepath.Join(f.store.Root, "tasks", secondID, "meta.json")
	data, err := os.ReadFile(metaPath)
	require(t, err)
	var meta task.MetaRecord
	require(t, json.Unmarshal(data, &meta))
	meta.CreatedAt = "2026-04-01T00:00:00Z"
	changed, err := task.MarshalCanonical(meta)
	require(t, err)
	require(t, os.WriteFile(metaPath, changed, 0o600))
	progress, err := c.Scan(t.Context())
	require(t, err)
	if !progress.Incomplete || progress.Faults != 1 {
		t.Fatalf("immutable mutation accepted: %+v", progress)
	}
	page, err := c.Query(t.Context(), Query{})
	require(t, err)
	if len(page.Rows) != 2 {
		t.Fatal("faulted scan swept unseen data")
	}
	requestPath := filepath.Join(f.store.Root, "tasks", secondID, "task.json")
	request, err := os.ReadFile(requestPath)
	require(t, err)
	require(t, os.Remove(requestPath))
	progress, err = c.Scan(t.Context())
	require(t, err)
	if !progress.Incomplete || progress.Faults != 1 || progress.Indexed != 2 {
		t.Fatalf("a partial read cleared a previously observed immutable drift fault: %+v", progress)
	}
	require(t, os.WriteFile(requestPath, request, 0o600))
	require(t, os.WriteFile(metaPath, data, 0o600))
	_, err = c.Scan(t.Context())
	require(t, err)
	page, err = c.Query(t.Context(), Query{})
	require(t, err)
	if len(page.Rows) != 1 || page.Rows[0].TaskID != secondID || page.Rows[0].Fault != "" {
		t.Fatalf("clean reconciliation did not remove absent row: %+v", page.Rows)
	}
}

func TestCatalogEntryFaultsDoNotBlockCompletedEnumeration(t *testing.T) {
	f := newSource(t)
	absent := f.task(t, 1, "2026-01-01T00:00:00Z")
	healthy := f.task(t, 2, "2026-01-02T00:00:00Z")
	partial := f.task(t, 3, "2026-01-03T00:00:00Z")
	c := openCatalog(t, f.options)
	previous, err := c.Scan(t.Context())
	require(t, err)
	requestPath := filepath.Join(f.store.Root, "tasks", partial, "task.json")
	request, err := os.ReadFile(requestPath)
	require(t, err)
	require(t, os.Remove(requestPath))
	require(t, os.RemoveAll(filepath.Join(f.store.Root, "tasks", absent)))
	junk := filepath.Join(f.store.Root, "tasks", ".DS_Store")
	require(t, os.WriteFile(junk, []byte("entry without task identity"), 0o600))
	for range 2 {
		progress, scanErr := c.Scan(t.Context())
		require(t, scanErr)
		if progress.Incomplete || progress.Faults != 2 || progress.LastScan.Equal(previous.LastScan) {
			t.Fatalf("entry faults poisoned completed enumeration: %+v", progress)
		}
		page, queryErr := c.Query(t.Context(), Query{})
		require(t, queryErr)
		if len(page.Rows) != 2 || page.Rows[0].TaskID != partial || page.Rows[1].TaskID != healthy || page.Rows[0].Fault == "" || page.Rows[0].Metadata.Provider != "fixture:test" {
			t.Fatalf("scan swept a present partial entry or retained independently absent data: %+v", page.Rows)
		}
		previous = progress
	}
	require(t, os.Remove(junk))
	require(t, os.WriteFile(requestPath, request, 0o600))
	progress, err := c.Scan(t.Context())
	require(t, err)
	if progress.Incomplete || progress.Faults != 0 || progress.Indexed != 2 {
		t.Fatalf("completed admission did not clear the entry fault on a later pass: %+v", progress)
	}
}

func TestPartialRemoteReferenceCompletesDiscoveryButUnsafeEntriesPreventSweep(t *testing.T) {
	f := newSource(t)
	local := f.task(t, 1, "2026-01-01T00:00:00Z")
	c := openCatalog(t, f.options)
	_, err := c.Scan(t.Context())
	require(t, err)
	id := fmt.Sprintf("%032x", 2)
	reference, err := remote.OpenReference(t.Context(), f.store, id, true)
	require(t, err)
	t.Cleanup(func() { require(t, reference.Close()) })
	// Opening the origin reference precedes SaveIntent's chunks-then-intent
	// commit. A directory without intent is a supported interrupted window.
	for range 2 {
		progress, scanErr := c.Scan(t.Context())
		require(t, scanErr)
		if progress.Incomplete || progress.Indexed != 2 || progress.Faults != 1 || progress.LastScan.IsZero() {
			t.Fatalf("partial remote intent blocked completed discovery: %+v", progress)
		}
	}
	unsafe := filepath.Join(f.store.Root, "remote-refs", fmt.Sprintf("%032x", 3))
	require(t, os.Symlink(t.TempDir(), unsafe))
	require(t, os.RemoveAll(filepath.Join(f.store.Root, "tasks", local)))
	progress, err := c.Scan(t.Context())
	require(t, err)
	if !progress.Incomplete || progress.Indexed != 3 || progress.Faults != 2 {
		t.Fatalf("unsafe remote entry no longer guarded reconciliation: %+v", progress)
	}
	require(t, os.Remove(unsafe))
	progress, err = c.Scan(t.Context())
	require(t, err)
	if progress.Incomplete || progress.Indexed != 1 || progress.Faults != 1 {
		t.Fatalf("remote partial entry blocked separately confirmed absence: %+v", progress)
	}
	brief := []byte("remote partial fixture")
	intent := remote.Intent{
		Version: remote.ProtocolVersion, OriginRootID: f.store.RootID, TaskID: id, Device: "fixture-device", Executable: "delegate",
		Destination: remote.Identity{Host: "fixture", Port: 22, User: "fixture", HostKey: "pinned", Root: f.cwd, RootID: strings.Repeat("b", 32)},
		Provider:    "fixture:test", Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}, RemoteCwd: f.cwd,
		Brief: remote.Descriptor{Name: "brief.md", Length: int64(len(brief)), SHA256: task.ComputeSHA256(brief)},
	}
	require(t, reference.SaveIntent(t.Context(), intent, brief))
	progress, err = c.Scan(t.Context())
	require(t, err)
	if progress.Incomplete || progress.Faults != 0 || progress.Indexed != 1 {
		t.Fatalf("committed remote intent did not clear the partial-entry fault: %+v", progress)
	}
}

func TestCatalogScanReleasesTemporarySeenOnSuccessAndCancellation(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%t", interrupted), func(t *testing.T) {
			f := newSource(t)
			f.task(t, 1, "2026-01-01T00:00:00Z")
			c := openCatalog(t, f.options)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if interrupted {
				c.afterScanBatch = func(context.Context) error { cancel(); return ctx.Err() }
			}
			_, err := c.Scan(ctx)
			if interrupted && !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted pass result: %v", err)
			} else if !interrupted {
				require(t, err)
			}
			assertNoTemporarySeen(t, c)
		})
	}
}

func assertNoTemporarySeen(t *testing.T, c *Catalog) {
	t.Helper()
	// Hold every configured physical connection concurrently. The assertion
	// must not depend on the pool choosing the previous scan connection first.
	var connections []*sql.Conn
	defer func() {
		for _, conn := range connections {
			require(t, conn.Close())
		}
	}()
	for range c.db.Stats().MaxOpenConnections {
		conn, err := c.db.Conn(t.Context())
		require(t, err)
		connections = append(connections, conn)
		var count int
		require(t, conn.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sqlite_temp_master WHERE name='seen'").Scan(&count))
		if count != 0 {
			t.Fatal("owned scan retained its temporary seen table on a pooled connection")
		}
	}
}

func TestCatalogRootReplacementInvalidatesReadAndAbsentRootIsNotCreated(t *testing.T) {
	f := newSource(t)
	c := openCatalog(t, f.options)
	oldPath := f.store.Root + "-old"
	require(t, os.Rename(f.store.Root, oldPath))
	replacement, err := taskdir.InitStore(f.store.Root)
	require(t, err)
	t.Cleanup(func() { require(t, replacement.Close()) })
	if _, err = c.Query(t.Context(), Query{}); !errors.Is(err, ErrRootChanged) {
		t.Fatalf("old root binding still usable: %v", err)
	}
	absent := f.options
	absent.Root = filepath.Join(t.TempDir(), "not-created")
	if _, err = Open(t.Context(), absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent root result: %v", err)
	}
	if _, err = os.Lstat(absent.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("history initialized absent execution root")
	}
}

func TestCatalogObservationBatchPreservesEntryFaultsAndRejectsBindingChange(t *testing.T) {
	f := newSource(t)
	id := f.task(t, 1, "2026-01-01T00:00:00Z")
	c := openCatalog(t, f.options)
	ids := []Identity{{Kind: taskdir.IndexLocal, TaskID: id}, {Kind: taskdir.IndexLocal, TaskID: fmt.Sprintf("%032x", 2)}}
	results, err := c.ObserveBatch(t.Context(), ids)
	require(t, err)
	if len(results) != 2 || results[0].Err != nil || results[0].Observation.At.IsZero() || !errors.Is(results[1].Err, os.ErrNotExist) {
		t.Fatalf("visible observation batch lost ordered facts/faults: %+v", results)
	}
	if _, err = c.ObserveBatch(t.Context(), make([]Identity, maximumPageSize+1)); err == nil {
		t.Fatal("visible observation accepted an unbounded batch")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = c.ObserveBatch(ctx, ids); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled batch became an entry fault: %v", err)
	}
	require(t, os.Rename(f.store.Root, f.store.Root+"-old"))
	replacement, err := taskdir.InitStore(f.store.Root)
	require(t, err)
	t.Cleanup(func() { require(t, replacement.Close()) })
	if values, batchErr := c.ObserveBatch(t.Context(), ids); !errors.Is(batchErr, ErrRootChanged) || values != nil {
		t.Fatalf("changed source was swallowed as visible row faults: %+v %v", values, batchErr)
	}
}

func TestCatalogRemoteDatesAndPassiveCachedObservation(t *testing.T) {
	f := newSource(t)
	id := f.task(t, 1, "2026-01-01T00:00:00Z")
	brief := []byte("brief")
	identity := remote.Identity{Host: "fixture", Port: 22, User: "fixture", HostKey: "pinned", Root: f.cwd, RootID: strings.Repeat("b", 32)}
	intent := remote.Intent{
		Version: remote.ProtocolVersion, OriginRootID: f.store.RootID, TaskID: id, Device: "fixture-device", Executable: "delegate", Destination: identity,
		Provider: "fixture:test", Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}, RemoteCwd: f.cwd,
		Brief: remote.Descriptor{Name: "brief.md", Length: int64(len(brief)), SHA256: task.ComputeSHA256(brief)},
	}
	reference, err := remote.OpenReference(t.Context(), f.store, id, true)
	require(t, err)
	require(t, reference.SaveIntent(t.Context(), intent, brief))
	envelope, err := json.Marshal(map[string]string{"task_id": id, "root_id": identity.RootID, "admission": "admitted", "liveness": "undetermined", "publication": "committed", "private_payload": "do-not-index-provider-content"})
	require(t, err)
	require(t, reference.Observe(t.Context(), remote.Response{Version: remote.ProtocolVersion, Identity: identity, Envelope: envelope}))
	require(t, reference.Close())
	before := snapshot(t, f.store.Root)
	c := openCatalog(t, f.options)
	_, err = c.Scan(t.Context())
	require(t, err)
	_, err = c.RefreshObservations(t.Context(), 10)
	require(t, err)
	page, err := c.Query(t.Context(), Query{})
	require(t, err)
	if len(page.Rows) != 2 || page.Rows[0].Kind != taskdir.IndexLocal || page.Rows[1].Kind != taskdir.IndexRemote || page.Rows[1].Metadata.CreatedAtKnown {
		t.Fatalf("remote date invented or equal IDs merged: %+v", page.Rows)
	}
	page, err = c.Query(t.Context(), Query{Publication: "committed"})
	require(t, err)
	if len(page.Rows) != 1 || page.Rows[0].Observation.Source != "origin-cache" || page.Progress.Unobserved != 0 {
		t.Fatalf("cached observation/filter lost: %+v", page)
	}
	if !reflect.DeepEqual(before, snapshot(t, f.store.Root)) {
		t.Fatal("cached remote observation wrote origin state")
	}
}

func TestCatalogCacheFailureModesAndQueryBoundaries(t *testing.T) {
	f := newSource(t)
	f.task(t, 1, "2026-01-01T00:00:00Z")
	c := openCatalog(t, f.options)
	path := c.CachePath()
	require(t, c.Close())
	require(t, os.WriteFile(path, []byte("invalid SQLite"), 0o600))
	repaired := openCatalog(t, f.options)
	if !repaired.Rebuilt() {
		t.Fatal("corrupt cache rebuild was not visible")
	}
	for _, query := range []Query{{Limit: -1}, {Limit: 201}, {Order: "invalid"}, {After: "invalid"}, {Publication: "invented"}} {
		if _, err := repaired.Query(t.Context(), query); err == nil {
			t.Fatalf("invalid query accepted: %+v", query)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repaired.Scan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan continued: %v", err)
	}
	require(t, repaired.Close())
	require(t, os.Remove(path))
	outside := filepath.Join(t.TempDir(), "outside")
	require(t, os.WriteFile(outside, []byte("retain"), 0o600))
	require(t, os.Symlink(outside, path))
	if _, err := Open(t.Context(), f.options); err == nil {
		t.Fatal("cache symlink accepted")
	}
	data, err := os.ReadFile(outside)
	require(t, err)
	if string(data) != "retain" {
		t.Fatal("cache repair followed symlink")
	}
}

func TestCatalogRefusesCacheInsideExecutionRoot(t *testing.T) {
	f := newSource(t)
	options := f.options
	// HOME is used on macOS; XDG_CACHE_HOME is used on Linux.
	options.Environment = []string{"HOME=" + f.store.Root, "XDG_CACHE_HOME=" + f.store.Root}
	before := snapshot(t, f.store.Root)
	if _, err := Open(t.Context(), options); err == nil {
		t.Fatal("cache was written inside the authoritative execution root")
	}
	if !reflect.DeepEqual(before, snapshot(t, f.store.Root)) {
		t.Fatal("rejected cache location changed source state")
	}
}
