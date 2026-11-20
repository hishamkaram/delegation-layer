package taskdir

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	inspection "github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRootContextBindingRetainsOriginalToken(t *testing.T) {
	store := testStore(t)
	pin, err := store.PinRootContext(t.Context())
	must(t, err)
	origin := task.RootContextCoordinates{Device: pin.Device() + 1, Inode: pin.Inode()}
	expected, err := task.ConnectorRootToken(store.RootID, origin)
	must(t, err)
	record, err := readStoreRootRecord(t.Context(), store)
	must(t, err)
	record.RequiredFeatures = append(record.RequiredFeatures, task.RootContextBindingFeature)
	record.ContextBinding = &task.RootContextBinding{Version: 1, Root: pin.Root(), Origin: origin, Accepted: task.RootContextCoordinates{Device: pin.Device(), Inode: pin.Inode()}, Challenge: strings.Repeat("a", 64), ConfirmedAt: "2026-11-20T00:00:00Z"}
	data, err := task.MarshalCanonical(record)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(store.Root, "root.json"), data, 0o600))
	actual, err := store.RootTokenContext(t.Context())
	must(t, err)
	if actual != expected {
		t.Fatalf("root context binding did not retain original token: got %s, want %s", actual, expected)
	}
}

func TestRootContextMatchingLegacyDoesNotEnableFeature(t *testing.T) {
	store := testStore(t)
	pin, err := store.PinRootContext(t.Context())
	must(t, err)
	token, err := store.RootTokenContext(t.Context())
	must(t, err)
	before := snapshotTree(t, store.Root)
	reader := rootContextReader(t, store)
	_, err = reader.PrepareRootContextReconciliation(t.Context(), token, rootContextCoordinates(pin))
	if !errors.Is(err, ErrRootContextNotRequired) {
		t.Fatalf("matching legacy root proposed cutover: %v", err)
	}
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("unnecessary prepare changed root")
	}
}

type rootContextTestGate func(context.Context, RootPin, func(context.Context) error) error

func (gate rootContextTestGate) WithQuiescentRoot(ctx context.Context, pin RootPin, operation func(context.Context) error) error {
	return gate(ctx, pin, operation)
}

func rootContextGate(t *testing.T, store *Store) RootContextReconciliationGate {
	t.Helper()
	return rootContextTestGate(func(ctx context.Context, pin RootPin, operation func(context.Context) error) error {
		actual, err := store.PinRootContext(ctx)
		if err != nil || !actual.Equal(pin) {
			return errors.Join(err, task.ErrIdentityMismatch)
		}
		// Successful SH acquisition proves the application gate is entered
		// before storage's EX acquisition, rather than after it.
		if err = store.maintLock.LockSHNonblockingContext(ctx); err != nil {
			t.Errorf("gate entered after maintenance EX: %v", err)
			return err
		}
		if err = store.maintLock.Unlock(); err != nil {
			return err
		}
		return operation(ctx)
	})
}

func rootContextOrigin(t *testing.T, store *Store) (task.RootContextCoordinates, string) {
	t.Helper()
	pin, err := store.PinRootContext(t.Context())
	must(t, err)
	origin := task.RootContextCoordinates{Device: pin.Device() + 13, Inode: pin.Inode()}
	token, err := task.ConnectorRootToken(store.RootID, origin)
	must(t, err)
	return origin, token
}

func rootContextReader(t *testing.T, store *Store) *ReadOnlyStore {
	t.Helper()
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, store.predicates)
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	return reader
}

func TestRootContextPrepareIsPassiveWithoutMaintenanceLock(t *testing.T) {
	store := testStore(t)
	origin, token := rootContextOrigin(t, store)
	must(t, os.Remove(filepath.Join(store.Root, ".maintenance.lock")))
	reader := rootContextReader(t, store)
	before := snapshotTree(t, store.Root)
	prepared, err := reader.PrepareRootContextReconciliation(t.Context(), token, origin)
	must(t, err)
	if prepared.RootID != store.RootID || prepared.TokenSHA256 != task.ComputeSHA256([]byte(token)) || prepared.Challenge == "" || prepared.AlreadyReconciled {
		t.Fatalf("invalid preparation: %+v", prepared)
	}
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("passive preparation changed state")
	}
}

func rootContextWriteTask(t *testing.T, store *Store, token string) *task.TaskRecord {
	t.Helper()
	request, _, _ := preparedInput(t, store)
	request.ConnectorContext = &task.ConnectorContext{RootToken: token, Project: task.DirectoryIdentity{Kind: task.PathDirectoryBinding, Path: request.CanonicalCwd}}
	must(t, task.ValidateTaskRecord(request))
	directory := filepath.Join(store.Root, "tasks", request.TaskID)
	must(t, os.MkdirAll(directory, 0o700))
	data, err := task.MarshalCanonical(request)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(directory, "task.json"), data, 0o600))
	return request
}

func rootContextWriteInspection(t *testing.T, store *Store, token string) string {
	t.Helper()
	request, _, _ := preparedInput(t, store)
	request.ConnectorContext = &task.ConnectorContext{RootToken: token, Project: task.DirectoryIdentity{Kind: task.PathDirectoryBinding, Path: request.CanonicalCwd}}
	data, err := task.MarshalCanonical(request)
	must(t, err)
	identity := newTaskID(t)
	base := t.TempDir()
	record := inspection.RequestRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: request.TaskID,
		OperationID: identity, Task: *request, TaskSHA256: task.ComputeSHA256(data),
		Group: inspection.GroupForRoot(store.RootID), Label: inspection.LabelForIdentity(store.RootID, identity),
		CreatedAt: "2026-11-20T00:00:00Z", Deadline: time.Date(2026, 11, 20, 0, 1, 0, 0, time.UTC).Format(time.RFC3339Nano),
		Binding: inspection.Binding{
			DefinitionRevision: "inspection-v1", DefinitionSHA256: strings.Repeat("a", 64),
			HelperExecutable: filepath.Join(base, "helper"), WorkerExecutable: filepath.Join(base, "worker"),
			ExecutableIdentity: task.ExecutableIdentityInstalledPathV1,
			Supervisor: task.SupervisorRef{
				ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, ClientExecutable: filepath.Join(base, "pueue"),
				ResolvedConfigSHA256: strings.Repeat("c", 64), ConfigPath: filepath.Join(base, "pueue.yml"),
				ConfigDigest: strings.Repeat("b", 64), Endpoint: filepath.Join(base, "pueue.sock"), ObservedVersion: "fixture-v1",
			},
		},
	}
	must(t, inspection.ValidateRequestRecord(record, store.RootID))
	data, err = task.MarshalCanonical(record)
	must(t, err)
	directory := filepath.Join(store.Root, "inspections", identity)
	must(t, os.MkdirAll(directory, 0o700))
	path := filepath.Join(directory, "request.json")
	must(t, os.WriteFile(path, data, 0o600))
	return path
}

func TestRootContextReconciliationPreservesHistoryAndReopens(t *testing.T) {
	store := testStore(t)
	currentToken := enableConnectorFixture(t, store)
	_, err := store.OriginBarrierContext(t.Context(), "gateway", "recovery", "", "generation", currentToken)
	must(t, err)
	closed := sealedTask(t, store, "saved answer")
	collectOK(t, closed)
	origin, token := rootContextOrigin(t, store)
	request := rootContextWriteTask(t, store, token)
	request.Origin = &task.GatewayOrigin{GatewayID: "gateway", AdmissionGeneration: "generation", OperationID: "operation", PrincipalRef: "principal", GatewayRequestSHA256: strings.Repeat("a", 64)}
	requestBytes, err := task.MarshalCanonical(request)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(store.Root, "tasks", request.TaskID, "task.json"), requestBytes, 0o600))
	inspectionPath := rootContextWriteInspection(t, store, token)
	identity, err := store.RecordedTaskIdentityContext(t.Context(), request.TaskID)
	must(t, err)
	receipt, err := identity.ConnectorReceipt()
	must(t, err)
	before := rootContextEvidenceBytes(t, store.Root)
	reader := rootContextReader(t, store)
	prepared, err := reader.PrepareRootContextReconciliation(t.Context(), token, origin)
	must(t, err)
	if prepared.TaskRequests != 2 || prepared.InspectionRequests != 1 {
		t.Fatalf("incomplete request inventory: %+v", prepared)
	}
	_, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, rootContextGate(t, store))
	must(t, err)
	after := rootContextEvidenceBytes(t, store.Root)
	delete(before, "root.json")
	delete(after, "root.json")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reconciliation changed historical evidence")
	}
	if after[filepath.ToSlash(strings.TrimPrefix(inspectionPath, store.Root+string(filepath.Separator)))] == nil {
		t.Fatal("inspection evidence disappeared")
	}
	afterIdentity, err := store.RecordedTaskIdentityContext(t.Context(), request.TaskID)
	must(t, err)
	if !bytes.Equal(identity.data, afterIdentity.data) {
		t.Fatal("historical immutable request changed")
	}
	afterReceipt, err := reader.ReadConnectorReceiptContext(t.Context(), request.TaskID)
	must(t, err)
	if !reflect.DeepEqual(receipt, afterReceipt) {
		t.Fatal("historical connector receipt changed")
	}
	reopened, err := OpenStore(store.Root)
	must(t, err)
	defer func() { must(t, reopened.Close()) }()
	actual, err := reopened.RootTokenContext(t.Context())
	must(t, err)
	if actual != token {
		t.Fatal("reopen lost the retained token")
	}
	committed, err := reopened.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, rootContextGate(t, reopened))
	must(t, err)
	if !committed.AlreadyReconciled {
		t.Fatal("matching committed challenge was not idempotent")
	}
	must(t, reopened.EnableWorkspaceConnectorContext(t.Context(), true, true))
	actual, err = reopened.RootTokenContext(t.Context())
	must(t, err)
	if actual != token {
		t.Fatal("later root feature upgrade lost the binding")
	}
}

func rootContextEvidenceBytes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	must(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = data
		return nil
	}))
	return files
}

func TestRootContextInventoryRejectsMixedAndPartialHistory(t *testing.T) {
	for _, family := range []string{"tasks", "inspections"} {
		t.Run(family, func(t *testing.T) {
			store := testStore(t)
			origin, token := rootContextOrigin(t, store)
			path := ""
			if family == "tasks" {
				request := rootContextWriteTask(t, store, token)
				path = filepath.Join(store.Root, "tasks", request.TaskID, "task.json")
			} else {
				path = rootContextWriteInspection(t, store, token)
			}
			reader := rootContextReader(t, store)
			_, err := reader.PrepareRootContextReconciliation(t.Context(), token, origin)
			must(t, err)
			original, err := os.ReadFile(path)
			must(t, err)
			mixed := rootContextMixedRequest(t, family, original, token)
			must(t, os.WriteFile(path, mixed, 0o600))
			if _, err = reader.PrepareRootContextReconciliation(t.Context(), token, origin); err == nil {
				t.Fatal("mixed token history was accepted")
			}
			must(t, os.WriteFile(path, original, 0o600))
			must(t, os.WriteFile(filepath.Join(filepath.Dir(path), "stage.unresolved.tmp"), []byte("partial"), 0o600))
			if _, err = reader.PrepareRootContextReconciliation(t.Context(), token, origin); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("unresolved stage was accepted: %v", err)
			}
			must(t, os.Remove(filepath.Join(filepath.Dir(path), "stage.unresolved.tmp")))
			must(t, os.Remove(path))
			if _, err = reader.PrepareRootContextReconciliation(t.Context(), token, origin); err == nil {
				t.Fatal("missing canonical request was accepted")
			}
		})
	}
}

func rootContextMixedRequest(t *testing.T, family string, original []byte, token string) []byte {
	t.Helper()
	mixed := bytes.ReplaceAll(original, []byte(token), []byte(strings.Repeat("f", 64)))
	if family == "inspections" {
		// Keep the request valid so this case reaches the token comparison,
		// rather than merely failing the inspection's embedded-task checksum.
		var request inspection.RequestRecord
		must(t, task.DecodeStrict(mixed, &request))
		data, err := task.MarshalCanonical(request.Task)
		must(t, err)
		request.TaskSHA256 = task.ComputeSHA256(data)
		must(t, inspection.ValidateRequestRecord(request, request.RootID))
		mixed, err = task.MarshalCanonical(request)
		must(t, err)
	}
	return mixed
}

func TestRootContextConfirmationRefusesStaleChallengeAndRequiresGate(t *testing.T) {
	store := testStore(t)
	origin, token := rootContextOrigin(t, store)
	reader := rootContextReader(t, store)
	prepared, err := reader.PrepareRootContextReconciliation(t.Context(), token, origin)
	must(t, err)
	gate := rootContextGate(t, store)
	before := snapshotTree(t, store.Root)
	if _, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, false, gate); err == nil {
		t.Fatal("confirmation accepted missing offline attestation")
	}
	if _, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, nil); err == nil {
		t.Fatal("confirmation accepted missing runtime gate")
	}
	if !reflect.DeepEqual(before, snapshotTree(t, store.Root)) {
		t.Fatal("refused confirmation changed state")
	}
	rootContextWriteTask(t, store, token)
	if _, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, gate); !errors.Is(err, task.ErrRequestConflict) {
		t.Fatalf("stale history challenge was accepted: %v", err)
	}
	prepared, err = reader.PrepareRootContextReconciliation(t.Context(), token, origin)
	must(t, err)
	other, err := OpenStore(store.Root)
	must(t, err)
	defer func() { must(t, other.Close()) }()
	must(t, other.maintLock.LockSHNonblocking())
	_, err = store.ConfirmRootContextReconciliation(t.Context(), token, origin, prepared.Challenge, true, gate)
	must(t, other.maintLock.Unlock())
	if !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("confirmation bypassed real maintenance SH: %v", err)
	}
}
