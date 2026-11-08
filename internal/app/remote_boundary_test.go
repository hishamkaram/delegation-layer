package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app/remotecommands"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRemoteActualStoreOpenRejectsReplacedRoot(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), fixture.store.Root, fixture.deps.storeDependencies().registry())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	pin, err := reader.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	root := fixture.store.Root
	moved := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-original")
	if err = os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if removeErr := os.RemoveAll(root); removeErr != nil {
			t.Error(removeErr)
		}
		if renameErr := os.Rename(moved, root); renameErr != nil {
			t.Error(renameErr)
		}
	}()
	replacement, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = replacement.Close(); err != nil {
		t.Fatal(err)
	}
	deps := fixture.deps
	deps.remoteRootPin = &pin
	store, openErr := openStoreContext(t.Context(), root, deps.storeDependencies(), true)
	if store != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if openErr == nil {
		t.Fatal("replacement root granted mutation handle")
	}
	request := fixture.request(remote.Reconcile)
	response := remoteFeature(deps).Execute(t.Context(), request)
	if response.Failure == nil {
		t.Fatal("replacement root granted remote recovery")
	}
}

func TestRemoteChangedPinRequiresMatchingImmutableIntent(t *testing.T) {
	fixture := newRemoteServerFixture(t, true)
	for _, digest := range []string{"", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		request := fixture.request(remote.Status)
		changed := *request.Expected
		changed.RootInode++
		request.Expected = &changed
		request.IntentSHA256 = digest
		before := remoteStoreInventory(t, fixture.store.Root)
		response := remoteFeature(fixture.deps).Execute(t.Context(), request)
		if response.Failure == nil {
			t.Fatalf("mismatched intent exposed evidence: %+v", response)
		}
		if len(response.Envelope) != 0 {
			t.Fatal("mismatched intent retained evidence envelope")
		}
		if before != remoteStoreInventory(t, fixture.store.Root) {
			t.Fatal("passive mismatch wrote state")
		}
	}
}

func TestRemoteChangedPinRetainsInteractionEvents(t *testing.T) {
	fixture := remoteInteractiveBoundaryFixture(t)
	request := fixture.request(remote.Events)
	expected := *request.Expected
	expected.RootInode++
	request.Expected = &expected
	before := remoteStoreInventory(t, fixture.store.Root)
	response := remoteFeature(fixture.deps).Execute(t.Context(), request)
	envelope := remoteServerEnvelope(t, response)
	if envelope.Interaction == nil || envelope.Interaction.TurnID != "turn-1" || len(envelope.Events) != 1 || envelope.Events[0].Kind != "ready" || envelope.Cursor == nil || *envelope.Cursor != 1 {
		t.Fatalf("passive interaction evidence lost: %+v", envelope)
	}
	if before != remoteStoreInventory(t, fixture.store.Root) {
		t.Fatal("passive interaction read wrote state")
	}
}

func remoteInteractiveBoundaryFixture(t *testing.T) *remoteServerFixture {
	t.Helper()
	fixture := newRemoteServerFixture(t, false)
	request, meta, err := fixture.td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	fixture.intent.TaskID = "ffffffffffffffffffffffffffffffff"
	fixture.intent.Config.Interactive = true
	fixture.digest, err = remote.IntentDigest(fixture.intent)
	if err != nil {
		t.Fatal(err)
	}
	request.TaskID = fixture.intent.TaskID
	request.RequestedConfig = fixture.intent.Config
	request.Remote.IntentSHA256 = fixture.digest
	meta.TaskID = request.TaskID
	meta.RequestedConfig = request.RequestedConfig
	meta.SpecSHA256 = ""
	td, err := fixture.store.CreateTask(request.TaskID, request, fixture.brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordStarted(0); err != nil {
		t.Fatal(err)
	}
	if err = td.BeginInteraction(); err != nil {
		t.Fatal(err)
	}
	if err = td.RecordInteraction(task.InteractionEvent{Kind: "ready", TurnID: "turn-1", Capabilities: &task.InteractionCapabilities{Clarification: true, Steering: true}}); err != nil {
		t.Fatal(err)
	}
	fixture.td = td
	return fixture
}

func TestRemoteContextCommandsHonorExpiredOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := filepath.Join(t.TempDir(), "uninitialized")
	arguments := Arguments{Root: root, TaskID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	for _, command := range []string{"status", "collect", "cancel", "events"} {
		t.Run(command, func(t *testing.T) {
			arguments.Command = command
			result := runCommandContext(ctx, arguments, Dependencies{})
			if !errors.Is(result.err, context.Canceled) {
				t.Fatalf("command ignored operation cancellation: %v", result.err)
			}
			if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canceled command initialized root: %v", err)
			}
		})
	}
}

func TestRemoteExistingOperationsRejectDifferentOriginIntent(t *testing.T) {
	fixture := newRemoteServerFixture(t, true)
	operations := []remote.Operation{remote.Status, remote.Collect, remote.Events, remote.Cancel, remote.Reply, remote.Steer, remote.Download}
	for _, operation := range operations {
		t.Run(string(operation), func(t *testing.T) {
			request := fixture.request(operation)
			request.IntentSHA256 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			request.Control = &remote.Control{ID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}
			request.Transfer = &remote.TransferRequest{Selector: "payload"}
			before := remoteStoreInventory(t, fixture.store.Root)
			response := remoteFeature(fixture.deps).Execute(t.Context(), request)
			if response.Failure == nil || response.Failure.Code != "identity_mismatch" || len(response.Envelope) != 0 {
				t.Fatalf("wrong-origin operation accepted: %+v", response)
			}
			if before != remoteStoreInventory(t, fixture.store.Root) {
				t.Fatal("wrong-origin operation wrote state")
			}
		})
	}
}

func TestRemoteRecordedRootRejectsPathAlias(t *testing.T) {
	fixture := newRemoteServerFixture(t, true)
	request := fixture.request(remote.Status)
	request.Root = fixture.store.Root + string(os.PathSeparator) + "."
	response := remoteFeature(fixture.deps).Execute(t.Context(), request)
	if response.Failure == nil || response.Failure.Code != "identity_mismatch" {
		t.Fatalf("root alias accepted: %+v", response)
	}
}

func TestRemoteEarlyAdmissionFailureProvesNoCreation(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	request := fixture.request(remote.Admit)
	changed := *request.Expected
	changed.RootID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	request.Expected = &changed
	intent := *request.Intent
	intent.Destination = changed
	request.Intent = &intent
	before := remoteStoreInventory(t, fixture.store.Root)
	response := remoteFeature(fixture.deps).Execute(t.Context(), request)
	if response.TaskRecord != "not_created" || response.Failure == nil || response.Failure.Code != "identity_mismatch" || response.Existing {
		t.Fatalf("early admission error lost creation proof: %+v", response)
	}
	if before != remoteStoreInventory(t, fixture.store.Root) {
		t.Fatal("early rejected admission wrote state")
	}
	request.Operation = remote.Reconcile
	response = remoteFeature(fixture.deps).Execute(t.Context(), request)
	if response.TaskRecord == "not_created" {
		t.Fatal("reconcile claimed no creation")
	}
}

func TestRemoteEarlyInvalidBriefIsNotCreated(t *testing.T) {
	fixture := newRemoteServerFixture(t, false)
	request := fixture.request(remote.Admit)
	request.Data = []byte("changed brief")
	before := remoteStoreInventory(t, fixture.store.Root)
	response := remoteFeature(fixture.deps).Execute(t.Context(), request)
	if response.TaskRecord != "not_created" || response.Failure == nil || response.Failure.Code != "brief_mismatch" {
		t.Fatalf("invalid brief creation state: %+v", response)
	}
	if before != remoteStoreInventory(t, fixture.store.Root) {
		t.Fatal("invalid brief mutated state")
	}
}

func TestRemoteFinalPinUncertaintyUsesPendingCLIExit(t *testing.T) {
	response := remote.Response{Version: remote.ProtocolVersion, TaskRecord: "unknown", Failure: remote.FailureResponse("acceptance_unknown", remote.ErrAcceptanceUnknown)}
	operationErr := &remote.OperationError{Code: response.Failure.Code, Message: response.Failure.Message}
	err := fmt.Errorf("reconciling remote: %w", operationErr)
	if code := remoteCLIResultCode("dispatch", remotecommands.Report{Remote: &response}, err); code != 3 {
		t.Fatalf("uncertain admission exit%d, want pending3", code)
	}
	definite := &remote.OperationError{Code: "request_conflict", Message: "different immutable request"}
	if code := remoteCLIResultCode("dispatch", remotecommands.Report{}, definite); code != 1 {
		t.Fatalf("definite conflict misclassified pending: %d", code)
	}
}
