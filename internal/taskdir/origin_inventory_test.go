package taskdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	emptyOriginMembershipSHA256   = "37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570"
	escapedOriginMembershipSHA256 = "f25a8326be1589f8908b513e178241d5d111977029bb69b98b3ab5a7d07a8632"
)

func inventoryTestOrigin(gateway, binding string) *task.GatewayOrigin {
	return &task.GatewayOrigin{GatewayID: gateway, AdmissionGeneration: "generation", OperationID: "operation", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64), Binding: binding}
}

func closedInventoryStore(t *testing.T) (*Store, string) {
	t.Helper()
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", root)
	must(t, err)
	return store, root
}

func TestOriginInventoryCanonicalMembershipGoldens(t *testing.T) {
	for _, mode := range []string{"empty", "filtered-only", "escaped-refusals"} {
		t.Run(mode, func(t *testing.T) {
			store, root := closedInventoryStore(t)
			expected := []OriginInventoryEntry{}
			golden := emptyOriginMembershipSHA256
			if mode != "empty" {
				_, err := store.FenceTaskNamespaceContext(t.Context(), strings.Repeat("3", 32), inventoryTestOrigin("foreign", "ignored"))
				must(t, err)
			}
			if mode == "escaped-refusals" {
				origin := inventoryTestOrigin("gateway", "<>&\u2028\u2029\n\t雪\x01")
				for _, refusal := range []struct {
					id     string
					origin *task.GatewayOrigin
				}{{strings.Repeat("2", 32), origin}, {strings.Repeat("1", 32), nil}} {
					_, err := store.FenceTaskNamespaceContext(t.Context(), refusal.id, refusal.origin)
					must(t, err)
				}
				expected = []OriginInventoryEntry{{TaskID: strings.Repeat("1", 32), Resolution: "permanently_refused"}, {TaskID: strings.Repeat("2", 32), Resolution: "permanently_refused", Origin: origin}}
				golden = escapedOriginMembershipSHA256
			}
			old, err := identityToken(expected)
			must(t, err)
			if old != golden {
				t.Fatal("legacy canonical array hash differs from independently frozen bytes")
			}
			inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 10, root)
			must(t, err)
			if inventory.MembershipSHA256 != golden || !inventory.Complete || inventory.CompletenessReceipt == "" || inventory.Entries == nil || len(inventory.Entries) != len(expected) {
				t.Fatalf("canonical inventory changed: %+v", inventory)
			}
		})
	}
}

// This observer samples live payload retention at an existing verification
// barrier. The allowance is a quarter of the decoded fixture payload, not an
// exact allocator byte count. IDs, the page and per-record scratch fit well
// below it; retaining every prior Origin.Binding does not.
type inventoryRetentionProbe struct {
	BaseFaultInjector
	directory string
	count     int
	seen      map[string]bool
	baseline  uint64
	growth    int64
	measured  bool
}

func (p *inventoryRetentionProbe) OnConnectorVerificationBarrier(path string) error {
	if filepath.Dir(path) != p.directory || filepath.Ext(path) != ".json" || p.seen[path] {
		return nil
	}
	p.seen[path] = true
	if len(p.seen) == p.count {
		runtime.GC()
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		p.growth, p.measured = int64(memory.HeapAlloc)-int64(p.baseline), true
	}
	return nil
}

func TestOriginInventoryPageRetainsOnlyBoundedPayload(t *testing.T) {
	const count, bindingBytes = 64, 64 << 10
	store, root := closedInventoryStore(t)
	for i := range count {
		_, err := store.FenceTaskNamespaceContext(t.Context(), fmt.Sprintf("%032x", i+1), inventoryTestOrigin("gateway", strings.Repeat("x", bindingBytes)))
		must(t, err)
	}
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	probe := &inventoryRetentionProbe{directory: filepath.Join(store.Root, "task-refusals"), count: count, seen: make(map[string]bool), baseline: memory.HeapAlloc}
	store.SetFaultInjector(probe)
	t.Cleanup(func() { store.SetFaultInjector(nil) })
	inventory, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 1, root)
	must(t, err)
	if !probe.measured || len(inventory.Entries) != 1 || inventory.Complete || inventory.NextCursor != fmt.Sprintf("%032x", 1) {
		t.Fatal("fixture did not scan the complete membership beyond its one-entry page")
	}
	t.Logf("live heap growth at final record barrier: %d; decoded fixture payload: %d", probe.growth, count*bindingBytes)
	if probe.growth > count*bindingBytes/4 {
		t.Fatal("membership scan retained prior decoded payloads beyond the bounded page")
	}
}

var _ ConnectorVerificationInjector = (*inventoryRetentionProbe)(nil)

func openInventoryOrigin(t *testing.T, store *Store, root, gateway string) {
	t.Helper()
	_, err := store.OriginBarrierContext(t.Context(), gateway, "recovery", "", "generation", root)
	must(t, err)
	page, err := store.OriginInventoryContext(t.Context(), gateway, "generation", "", 1, root)
	must(t, err)
	_, err = store.OriginReopenContext(t.Context(), gateway, "recovery", "generation", page.CompletenessReceipt, root)
	must(t, err)
}

func createInventoryTask(t *testing.T, store *Store, root, id string, origin *task.GatewayOrigin) OriginInventoryEntry {
	t.Helper()
	request, meta, brief := preparedInput(t, store)
	request.TaskID, meta.TaskID = id, id
	request.Origin = origin.Clone()
	if origin != nil {
		project, err := ObserveProjectContext(t.Context(), request.CanonicalCwd)
		must(t, err)
		request.ConnectorContext = &task.ConnectorContext{RootToken: root, Project: project}
	}
	td, err := store.CreateTaskContext(t.Context(), id, request, brief, meta)
	must(t, err)
	must(t, td.Close())
	if origin == nil {
		return OriginInventoryEntry{}
	}
	data, err := task.MarshalCanonical(request)
	must(t, err)
	project, err := ProjectToken(request.ConnectorContext.Project)
	must(t, err)
	receipt := &ConnectorReceipt{RootToken: root, ProjectToken: project, TaskID: id, Origin: *origin, SpecSHA256: task.ComputeSHA256(data), BriefSHA256: task.ComputeSHA256(brief), ExecutionOptions: ConnectorExecutionOptions{Provider: "fixture:test", Permission: "read-only", BudgetNanoseconds: request.BudgetNanos, Model: ConnectorRequestedSelection{Kind: ConnectorSelectionNativeDefault}, Effort: ConnectorRequestedSelection{Kind: ConnectorSelectionNativeDefault}}}
	return OriginInventoryEntry{Receipt: receipt, TaskID: id, Resolution: "existing", Origin: origin.Clone(), SpecSHA256: receipt.SpecSHA256, BriefSHA256: receipt.BriefSHA256}
}

func mixedOriginInventory(t *testing.T) (*Store, string, []OriginInventoryEntry) {
	t.Helper()
	store := testStore(t)
	root := enableConnectorFixture(t, store)
	openInventoryOrigin(t, store, root, "gateway")
	openInventoryOrigin(t, store, root, "foreign")
	local := createInventoryTask(t, store, root, strings.Repeat("1", 32), inventoryTestOrigin("gateway", "task:<>&\n雪"))
	createInventoryTask(t, store, root, strings.Repeat("2", 32), inventoryTestOrigin("foreign", "foreign"))
	refusalOrigin := inventoryTestOrigin("gateway", "refusal:<>&\t\u0001")
	_, err := store.FenceTaskNamespaceContext(t.Context(), strings.Repeat("3", 32), refusalOrigin)
	must(t, err)
	duplicate := strings.Repeat("4", 32)
	createInventoryTask(t, store, root, duplicate, nil)
	// A complete prepared task resolves as existing. Model an interrupted
	// preparation before asking the supported operator fence to retain it.
	must(t, os.Remove(filepath.Join(store.Root, "tasks", duplicate, "meta.json")))
	fenced, err := store.FencePartialTaskContext(t.Context(), duplicate, PartialFenceSelection{OperatorUnattributed: true})
	must(t, err)
	if fenced.Resolution != "permanently_refused" {
		t.Fatal("duplicate-ID fixture did not become a canonical refusal")
	}
	createInventoryTask(t, store, root, strings.Repeat("5", 32), nil)
	_, err = store.FenceTaskNamespaceContext(t.Context(), strings.Repeat("6", 32), inventoryTestOrigin("foreign", "foreign-refusal"))
	must(t, err)
	_, err = store.FenceTaskNamespaceContext(t.Context(), strings.Repeat("7", 32), nil)
	must(t, err)
	_, err = store.OriginBarrierContext(t.Context(), "gateway", "recovery-closed", "generation", "closed", root)
	must(t, err)
	return store, root, []OriginInventoryEntry{local, {TaskID: strings.Repeat("3", 32), Resolution: "permanently_refused", Origin: refusalOrigin}, {TaskID: duplicate, Resolution: "permanently_refused"}, {TaskID: strings.Repeat("7", 32), Resolution: "permanently_refused"}}
}

func TestOriginInventoryMixedPagesMatchLegacyHashAndReopenRetry(t *testing.T) {
	store, root, expected := mixedOriginInventory(t)
	membership, err := identityToken(expected)
	must(t, err)
	all, err := store.OriginInventoryContext(t.Context(), "gateway", "closed", "", 200, root)
	must(t, err)
	requireFullOriginInventory(t, all, expected, membership)
	cursor, receipt := "", ""
	for i, want := range expected {
		page, readErr := store.OriginInventoryContext(t.Context(), "gateway", "closed", cursor, 1, root)
		must(t, readErr)
		requireSingleOriginPage(t, page, want, membership, i == len(expected)-1)
		cursor, receipt = page.NextCursor, page.CompletenessReceipt
	}
	tail, err := store.OriginInventoryContext(t.Context(), "gateway", "closed", strings.Repeat("f", 32), 1, root)
	must(t, err)
	if tail.Entries == nil || len(tail.Entries) != 0 || !tail.Complete || tail.MembershipSHA256 != membership || tail.CompletenessReceipt != receipt {
		t.Fatal("cursor beyond membership changed the complete empty tail")
	}
	state, err := store.OriginReopenContext(t.Context(), "gateway", "recovery-closed", "closed", receipt, root)
	must(t, err)
	if !state.AdmissionOpen || state.MembershipSHA256 != membership {
		t.Fatal("reopen did not use the same canonical membership as inventory")
	}
	origin := inventoryTestOrigin("gateway", "after-open")
	origin.AdmissionGeneration = "closed"
	createInventoryTask(t, store, root, strings.Repeat("8", 32), origin)
	replayed, err := store.OriginReopenContext(t.Context(), "gateway", "recovery-closed", "closed", receipt, root)
	must(t, err)
	if !reflect.DeepEqual(state, replayed) {
		t.Fatal("opened receipt retry rescanned or rewrote admission after new tasks")
	}
}

func requireFullOriginInventory(t *testing.T, page OriginInventory, expected []OriginInventoryEntry, membership string) {
	t.Helper()
	if !reflect.DeepEqual(page.Entries, expected) {
		t.Fatal("full inventory differs from the independently constructed canonical union")
	}
	if page.MembershipSHA256 != membership || !page.Complete || page.NextCursor != "" || page.CompletenessReceipt == "" {
		t.Fatal("full inventory differs from legacy canonical array hash or completeness")
	}
}

func requireSingleOriginPage(t *testing.T, page OriginInventory, want OriginInventoryEntry, membership string, last bool) {
	t.Helper()
	if len(page.Entries) != 1 || !reflect.DeepEqual(page.Entries[0], want) || page.MembershipSHA256 != membership {
		t.Fatal("page lost a canonical union member or legacy membership")
	}
	if last {
		if !page.Complete || page.NextCursor != "" || page.CompletenessReceipt == "" {
			t.Fatal("last page did not preserve completeness")
		}
	} else if page.Complete || page.NextCursor != want.TaskID || page.CompletenessReceipt != "" {
		t.Fatal("page cursor or incomplete state changed")
	}
}

type inventoryReadFault struct {
	BaseFaultInjector
	path    string
	failure error
	cancel  context.CancelFunc
}

func (f *inventoryReadFault) OnConnectorVerificationBarrier(path string) error {
	if path == f.path {
		if f.cancel != nil {
			f.cancel()
		}
		return f.failure
	}
	return nil
}

func requireNoOriginInventoryProof(t *testing.T, page OriginInventory) {
	t.Helper()
	if page.Entries != nil || page.NextCursor != "" || page.Complete || page.MembershipSHA256 != "" || page.CompletenessReceipt != "" {
		t.Fatal("failed inventory released a partial page or completeness proof")
	}
}

func TestOriginInventoryPageDoesNotSkipSuffixFailures(t *testing.T) {
	for _, mode := range []string{"read-fault", "cancel", "malformed-foreign"} {
		t.Run(mode, func(t *testing.T) {
			store, root := closedInventoryStore(t)
			for _, id := range []string{strings.Repeat("1", 32), strings.Repeat("2", 32)} {
				_, err := store.FenceTaskNamespaceContext(t.Context(), id, inventoryTestOrigin("gateway", "payload"))
				must(t, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("inventory suffix read failed")
			fault := &inventoryReadFault{path: filepath.Join(store.Root, "task-refusals", strings.Repeat("2", 32)+".json"), failure: failure}
			switch mode {
			case "cancel":
				fault.failure, fault.cancel = nil, cancel
			case "malformed-foreign":
				id := strings.Repeat("3", 32)
				_, err := store.FenceTaskNamespaceContext(t.Context(), id, inventoryTestOrigin("foreign", "foreign"))
				must(t, err)
				must(t, os.WriteFile(filepath.Join(store.Root, "task-refusals", id+".json"), []byte("{"), 0o600))
				fault.path = ""
			}
			store.SetFaultInjector(fault)
			t.Cleanup(func() { store.SetFaultInjector(nil) })
			page, err := store.OriginInventoryContext(ctx, "gateway", "generation", "", 1, root)
			if err == nil || (mode == "read-fault" && !errors.Is(err, failure)) || (mode == "cancel" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("suffix failure was skipped: %v", err)
			}
			requireNoOriginInventoryProof(t, page)
		})
	}
}

func TestOriginInventoryDeferredCleanupClearsPageAndRetainsOwner(t *testing.T) {
	for _, name := range []string{".maintenance.lock", strings.Repeat("1", 32) + ".lock"} {
		t.Run(name, func(t *testing.T) {
			store, root := closedInventoryStore(t)
			_, err := store.FenceTaskNamespaceContext(t.Context(), strings.Repeat("1", 32), inventoryTestOrigin("gateway", "payload"))
			must(t, err)
			failure := errors.New("inventory owner cleanup failed")
			var release func() error
			t.Cleanup(func() {
				store.SetFaultInjector(nil)
				if release != nil {
					must(t, release())
				}
			})
			store.SetFaultInjector(&observationUnlockFault{name: name, failure: failure})
			page, err := store.OriginInventoryContext(t.Context(), "gateway", "generation", "", 1, root)
			var maintenance *ConnectorMaintenanceCleanupError
			var namespace *NamespaceCleanupError
			switch {
			case errors.As(err, &maintenance):
				release = maintenance.Release
				if maintenance.RetainedStore() != store {
					t.Fatal("maintenance cleanup lost its retained store")
				}
			case errors.As(err, &namespace):
				release = namespace.Release
				if namespace.RetainedStore() != store {
					t.Fatal("namespace cleanup lost its retained store")
				}
			default:
				t.Fatalf("cleanup owner was lost: %v", err)
			}
			if !errors.Is(err, failure) {
				t.Fatal("cleanup error cause was lost")
			}
			requireNoOriginInventoryProof(t, page)
		})
	}
}
