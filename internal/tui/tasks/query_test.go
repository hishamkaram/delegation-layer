package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/history"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func TestTaskPageObservedOrderUsesDatedRemoteEvidenceWithoutInventingCreation(t *testing.T) {
	store := newTaskHistoryStore(t, t.TempDir())
	old, newer := strings.Repeat("a", 32), strings.Repeat("b", 32)
	historicalRemote(t, store, old, "2026-01-01T00:00:00Z", "committed")
	historicalRemote(t, store, newer, "2026-01-03T00:00:00Z", "committed")
	catalog := openTaskHistoryCatalog(t, store)
	if _, err := catalog.ScanAndObserve(t.Context()); err != nil {
		t.Fatal(err)
	}
	section := NewSection(nil, app.Arguments{Root: store.Root}, "", catalog)
	for _, fixture := range []struct{ search, first, summary string }{
		{"order:created", "remote:" + old, "Order: created"},
		{"order:observed", "remote:" + newer, "Order: last observed"},
	} {
		page, err := section.Load(t.Context(), ui.PageRequest{Search: fixture.search})
		if err != nil || len(page.Entries) != 2 {
			t.Fatalf("history page unavailable: %+v %v", page, err)
		}
		if page.Entries[0].ID != fixture.first || !strings.Contains(page.Coverage, fixture.summary) {
			t.Fatalf("order selector lost at actual page consumer: %+v", page)
		}
		for _, entry := range page.Entries {
			if got := entryFields(entry)["Identity/Created"]; got != "unknown" {
				t.Fatalf("observation time became creation time: %q", got)
			}
		}
	}
}

func TestTaskEmptyHistoryHasTableAndDatedCoverage(t *testing.T) {
	store := newTaskHistoryStore(t, t.TempDir())
	catalog := openTaskHistoryCatalog(t, store)
	if _, err := catalog.ScanAndObserve(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := NewSection(nil, app.Arguments{Root: store.Root}, "", catalog).Load(t.Context(), ui.PageRequest{})
	if err != nil || len(page.Entries) != 0 {
		t.Fatalf("empty history page failed: %+v %v", page, err)
	}
	want := []ui.Column{{Title: "Task ID", Width: 16}, {Title: "Provider", Width: 18}, {Title: "Admission", Width: 14}, {Title: "Liveness", Width: 14}, {Title: "Publication", Width: 14}, {Title: "Created", Width: 20}}
	if !slices.Equal(page.Columns, want) {
		t.Fatalf("empty available history lost the table contract: %+v", page.Columns)
	}
	if strings.Contains(page.Strip, "\n") || !strings.HasPrefix(page.Strip, "Dated • ") || !strings.Contains(page.Strip, "0 indexed") || !strings.Contains(page.Coverage, "Last full metadata scan:") || !strings.Contains(page.Coverage, "not current global status") || page.Summary != "" {
		t.Fatalf("empty table lost dated scan qualification: %+v", page)
	}
}

func TestTaskPageClearsRecoveredObservationFault(t *testing.T) {
	fixture := t.TempDir()
	store := newTaskHistoryStore(t, fixture)
	id, at := strings.Repeat("a", 32), "2026-01-02T03:00:00Z"
	observation := historicalRemote(t, store, id, at, "pending")
	catalog := openTaskHistoryCatalog(t, store)
	if _, err := catalog.ScanAndObserve(t.Context()); err != nil {
		t.Fatal(err)
	}
	hiddenObservation := filepath.Join(fixture, "hidden-observation.json")
	recordRemoteCatalogFaults(t, catalog, observation, hiddenObservation)

	section := NewSection(nil, app.Arguments{Root: store.Root}, "", catalog)
	faulted := loadRemoteTaskObservation(t, section, id, at)
	const metadataFault = "source metadata unavailable or invalid"
	for _, cell := range faulted.Cells[2:5] {
		if !cell.Fault {
			t.Fatalf("failed read did not mark retained lifecycle evidence: %+v", faulted.Cells)
		}
	}
	if faults := entryFaults(faulted); !slices.Equal(faults, []string{metadataFault, "visible passive refresh unavailable; previous evidence retained"}) {
		t.Fatalf("failed read lost fault details: %v", faults)
	}

	if err := os.Rename(hiddenObservation, observation); err != nil {
		t.Fatal(err)
	}
	recovered := loadRemoteTaskObservation(t, section, id, at)
	for _, cell := range recovered.Cells[2:5] {
		if cell.Fault {
			t.Errorf("successful read left stale lifecycle fault markers: %+v", recovered.Cells)
			break
		}
	}
	if faults := entryFaults(recovered); !slices.Equal(faults, []string{metadataFault}) {
		t.Errorf("successful read left stale observation fault details: %v", faults)
	}
	cached, err := catalog.Query(t.Context(), history.Query{})
	if err != nil || len(cached.Rows) != 1 || cached.Rows[0].ObservationFault == "" {
		t.Fatalf("visible recovery changed the catalog fault: %+v %v", cached, err)
	}
}

func newTaskHistoryStore(t *testing.T, fixture string) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(fixture, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store
}

func openTaskHistoryCatalog(t *testing.T, store *taskdir.Store) *history.Catalog {
	t.Helper()
	catalog, err := history.Open(t.Context(), history.Options{Root: store.Root, Environment: []string{"HOME=" + t.TempDir(), "XDG_CACHE_HOME=" + t.TempDir()}, Registry: predicate.Default()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := catalog.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return catalog
}

func recordRemoteCatalogFaults(t *testing.T, catalog *history.Catalog, observation, hiddenObservation string) {
	t.Helper()
	// Record the observation fault first so the separate metadata fault does
	// not make the row ineligible for catalog observation.
	if err := os.Rename(observation, hiddenObservation); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.RefreshObservations(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	intent := filepath.Join(filepath.Dir(observation), "intent.json")
	hiddenIntent := filepath.Join(filepath.Dir(hiddenObservation), "hidden-intent.json")
	if err := os.Rename(intent, hiddenIntent); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(hiddenIntent, intent); err != nil {
		t.Fatal(err)
	}
}

func loadRemoteTaskObservation(t *testing.T, section ui.Section, id, at string) ui.Entry {
	t.Helper()
	page, err := section.Load(t.Context(), ui.PageRequest{})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].ID != "remote:"+id {
		t.Fatalf("remote history row unavailable: %+v %v", page, err)
	}
	entry := page.Entries[0]
	fields := entryFields(entry)
	if fields["Evidence/Observed"] != at || fields["Evidence/Source"] != "origin-cache" || fields["Evidence/Publication"] != "pending" {
		t.Fatalf("remote observation facts changed: %+v", fields)
	}
	if !entry.Cells[0].Fault {
		t.Fatal("independent metadata fault marker was lost")
	}
	return entry
}

func historicalRemote(t *testing.T, store *taskdir.Store, id, at, publication string) string {
	t.Helper()
	brief := []byte("bounded fixture")
	destination := remote.Identity{Host: "fixture", Port: 22, User: "fixture", HostKey: "pinned", Root: t.TempDir(), RootID: strings.Repeat("c", 32)}
	intent := remote.Intent{Version: remote.ProtocolVersion, OriginRootID: store.RootID, TaskID: id, Device: "fixture", Executable: "delegate", Destination: destination, Provider: "fixture:test", Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}, RemoteCwd: destination.Root, Brief: remote.Descriptor{Name: "brief.md", Length: int64(len(brief)), SHA256: task.ComputeSHA256(brief)}}
	ref, err := remote.OpenReference(t.Context(), store, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = ref.SaveIntent(t.Context(), intent, brief); err != nil {
		t.Fatal(err)
	}
	if err = ref.Close(); err != nil {
		t.Fatal(err)
	}
	// Historical fixture timestamps are explicit record facts, independent of
	// wall-clock timing or incidental directory iteration on this host.
	records, err := store.OpenRemoteReferenceContext(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := records.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	envelope, err := json.Marshal(map[string]string{"task_id": id, "root_id": destination.RootID, "admission": "admitted", "liveness": "undetermined", "publication": publication})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = time.Parse(time.RFC3339, at); err != nil {
		t.Fatal(err)
	}
	const observationName = "observation-00000000000000000001.json"
	_, err = records.PutContext(t.Context(), observationName, remote.Observation{Sequence: 1, ObservedAt: at, Response: remote.Response{Version: remote.ProtocolVersion, Identity: destination, Envelope: envelope}})
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(store.Root, "remote-refs", id, observationName)
}
