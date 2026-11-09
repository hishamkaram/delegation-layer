package tasks

import (
	"encoding/json"
	"path/filepath"
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
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	old, newer := strings.Repeat("a", 32), strings.Repeat("b", 32)
	historicalRemote(t, store, old, "2026-01-01T00:00:00Z")
	historicalRemote(t, store, newer, "2026-01-03T00:00:00Z")
	catalog, err := history.Open(t.Context(), history.Options{Root: store.Root, Environment: []string{"HOME=" + t.TempDir(), "XDG_CACHE_HOME=" + t.TempDir()}, Registry: predicate.Default()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := catalog.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, err = catalog.ScanAndObserve(t.Context()); err != nil {
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
		if page.Entries[0].ID != fixture.first || !strings.Contains(page.Summary, fixture.summary) {
			t.Fatalf("order selector lost at actual page consumer: %+v", page)
		}
		for _, entry := range page.Entries {
			if !strings.Contains(entry.Detail, "Created: unknown") {
				t.Fatalf("observation time became creation time: %s", entry.Detail)
			}
		}
	}
}

func historicalRemote(t *testing.T, store *taskdir.Store, id, at string) {
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
	envelope, err := json.Marshal(map[string]string{"task_id": id, "root_id": destination.RootID, "admission": "admitted", "liveness": "undetermined", "publication": "committed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = time.Parse(time.RFC3339, at); err != nil {
		t.Fatal(err)
	}
	_, err = records.PutContext(t.Context(), "observation-00000000000000000001.json", remote.Observation{Sequence: 1, ObservedAt: at, Response: remote.Response{Version: remote.ProtocolVersion, Identity: destination, Envelope: envelope}})
	if err != nil {
		t.Fatal(err)
	}
}
