package app

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// recordClosedProvisionHelper gives the natively provisioned resource the
// closed provision helper that retained-checkout reuse requires, so a later
// dispatch can attach to it without scheduling a helper.
func recordClosedProvisionHelper(t *testing.T, fixture checkpointRuntimeFixture, resource *workspace.Resource) {
	t.Helper()
	store, request := fixture.lineage.store, resource.Request()
	operationID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	digest := task.ComputeSHA256([]byte("provision " + operationID))
	helper := workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: operationID, WorkspaceID: request.WorkspaceID,
		Kind: workspace.OperationProvision, Subject: workspace.Subject{Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: request.RequestSHA256},
		Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), WorkerExecutable: "/fixture/delegate-run", WorkerSHA256: digest,
		Supervisor: supervisorfixture.ContinuationSupervisorRef(store.Root, digest, "unix:/fixture/pueue.sock"),
	}
	operation, err := workspace.OpenOperation(t.Context(), store, helper)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordOperation(t.Context(), helper); err != nil {
		t.Fatal(err)
	}
	seedRealWorkspaceOperationClosure(t, store, operation)
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
}

// A fresh dispatch into a retained managed Git worktree takes its resource
// refreshing check after the workspace is reserved and the provider is
// prepared. Confirmed pressure there refuses before any task record, capture
// or submission, and the same dispatch's workspace admission closes exactly
// as for any other task that was never created: the reservation is aborted
// as not launched and the checkout is retained and reusable.
func TestFreshManagedWorkspaceDispatchLateResourcePressureClosesAdmission(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	resource, registration := fixture.lineage.provisionedChild(t, fixture.lineage.main)
	recordClosedProvisionHelper(t, fixture, resource)
	resources := &recordingResources{assessments: []func() resourceadmission.Assessment{clearStale, testPressureAssessment}}
	resources.t = t
	provider := &preflightFixture{}
	fixture.deps.ResourceChecker = resources
	fixture.deps.Catalog = preflightFixtureCatalog(t, provider.candidate)
	fixture.deps.RunnerExecutable = canonicalAppTestExecutable(t)

	result := dispatch(Arguments{
		Root: fixture.lineage.store.Root, WorkspaceID: resource.Request().WorkspaceID, Provider: "fixture:preflight",
		BriefBytes: []byte("bounded task"), Config: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "1m"},
	}, fixture.deps)

	if result.code != 1 {
		t.Fatalf("late pressure dispatch = %+v", result)
	}
	assertResourcePressureFailure(t, jsonResponseFor(t, "dispatch", result), "not_created", "retry_later", 30)
	if want := []string{"check", "scope", "check"}; !slices.Equal(resources.order, want) {
		t.Fatalf("resource calls = %v, want the initial check, the freshness scope, then the refreshed check", resources.order)
	}
	if provider.prepareCalls != 1 {
		t.Fatalf("provider preparation ran %d times, want once before the late refusal", provider.prepareCalls)
	}
	if entries := taskEntries(t, fixture.lineage.store.Root); len(entries) != 0 {
		t.Fatalf("late refusal created task entries: %v", entries)
	}
	fixture.requireAdds(t, 0)
	assertRefusedAdmissionClosed(t, resource, registration)
}

// assertRefusedAdmissionClosed checks the durable workspace state after the
// refused admission: its reservation is closed as never launched, no use owner
// remains, the retained checkout is untouched, and the next admission starts
// from a fresh reservation.
func assertRefusedAdmissionClosed(t *testing.T, resource *workspace.Resource, registration workspace.Registration) {
	t.Helper()
	closed, err := resource.ReservationClosed(t.Context(), 1)
	if err != nil || closed.Disposition != "aborted-not-launched" {
		t.Fatalf("refused admission closure = %+v, %v; want aborted-not-launched", closed, err)
	}
	if _, err = resource.Reservation(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("refused admission left an active reservation: %v", err)
	}
	if _, err = os.Stat(registration.CheckoutPath); err != nil {
		t.Fatalf("refused admission lost the retained checkout: %v", err)
	}
	owner, err := resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatalf("refused admission kept the workspace use owner: %v", err)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	subject := workspace.Subject{Kind: workspace.SubjectAdmission, ID: resource.Request().TaskID, SHA256: resource.Request().RequestSHA256}
	next, err := resource.Reserve(t.Context(), subject)
	if err != nil || next.Sequence != 2 {
		t.Fatalf("workspace is not reusable after the refusal: %+v, %v", next, err)
	}
}
