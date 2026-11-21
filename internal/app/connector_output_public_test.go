package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestConnectorPublicInventoryOverflowIsExplicitAndSmallerPagesRetainEvidence(t *testing.T) {
	store, root, entries := connectorPublicInventoryFixture(t)
	args := []string{"origin", "inventory", "--root", store.Root, "--gateway-id", "gateway", "--generation", "generation", "--expected-root", root, "--json"}
	var stdout, stderr bytes.Buffer
	code := Run(append(args, "--limit", "5"), &stdout, &stderr, Dependencies{Environment: []string{}})
	if code == 0 || stdout.Len() != 0 {
		t.Fatalf("oversized inventory emitted partial JSON or succeeded: exit=%d bytes=%d", code, stdout.Len())
	}
	if !strings.Contains(stderr.String(), "JSON response exceeds maximum permitted size") || stderr.Len() > 256 {
		t.Fatalf("public overflow lost its bounded diagnostic: exit=%d stderr=%q", code, &stderr)
	}
	var membership string
	cursor := ""
	for i, want := range entries {
		page := connectorPublicInventoryPage(t, args, cursor)
		if len(page.Entries) != 1 || !reflect.DeepEqual(page.Entries[0], want) {
			t.Fatal("count-one inventory trimmed or skipped canonical refusal evidence")
		}
		if i == 0 {
			membership = page.MembershipSHA256
		}
		if page.MembershipSHA256 == "" || page.MembershipSHA256 != membership || page.Complete != (i == len(entries)-1) {
			t.Fatal("count-one inventory changed membership or completeness")
		}
		cursor = page.NextCursor
	}
	if cursor != "" {
		t.Fatal("count-one inventory did not reach the canonical end")
	}
}

func connectorPublicInventoryFixture(t *testing.T) (*taskdir.Store, string, []taskdir.OriginInventoryEntry) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = store.EnableConnectorContext(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	root, err := store.RootTokenContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root); err != nil {
		t.Fatal(err)
	}
	entries := make([]taskdir.OriginInventoryEntry, 0, 5)
	for i := range 5 {
		id := fmt.Sprintf("%032x", i+1)
		origin := task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "operation-" + id, PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64), Binding: strings.Repeat("b", 900_000)}
		if err = task.ValidateGatewayOrigin(origin); err != nil {
			t.Fatal(err)
		}
		if _, err = store.FenceTaskNamespaceContext(t.Context(), id, &origin); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, taskdir.OriginInventoryEntry{TaskID: id, Resolution: "permanently_refused", Origin: &origin})
	}
	return store, root, entries
}

func connectorPublicInventoryPage(t *testing.T, args []string, cursor string) *taskdir.OriginInventory {
	t.Helper()
	arguments := append(args, "--limit", "1")
	if cursor != "" {
		arguments = append(arguments, "--cursor", cursor)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(arguments, &stdout, &stderr, Dependencies{Environment: []string{}}); code != 0 || stderr.Len() != 0 {
		t.Fatalf("count-one inventory failed: exit=%d stderr=%q", code, &stderr)
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Connector == nil || response.Connector.Inventory == nil {
		t.Fatal("count-one inventory omitted its canonical page")
	}
	page := response.Connector.Inventory
	if page.Complete && page.CompletenessReceipt == "" {
		t.Fatal("final count-one inventory omitted its completeness proof")
	}
	return page
}
