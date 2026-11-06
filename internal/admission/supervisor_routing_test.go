package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type admissionRoutingFunc func(context.Context, *taskdir.Store, infrastructure.LaunchContext) (infrastructure.Disposition, error)

func (f admissionRoutingFunc) EnsurePrivate(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, error) {
	return f(ctx, store, selected)
}

type admissionContinuationRouting struct {
	admissionRoutingFunc
	prepare func(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error)
}

func (r admissionContinuationRouting) PrepareContinuation(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, source infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	return r.prepare(ctx, store, selected, source)
}

func TestContinuationAdmissionKeepsOriginalSourceAfterCandidateMerge(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	clientPath := writeAdmissionRoutingClient(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	digest := task.ComputeSHA256([]byte("original metadata fixture"))
	original := supervisorfixture.ContinuationSupervisorRef(store.Root, digest, "unix:"+filepath.Join(store.Root, "original.sock"))
	meta := task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: request.RootID, TaskID: request.TaskID,
		RequestedConfig: request.RequestedConfig, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment: "fixture-only", Approval: "never", ProviderExecutable: original.ClientExecutable, ProviderVersion: "fixture-v2",
		PublisherBuild: "routing-fixture", PublisherVersion: "routing-fixture", Predicate: task.FixturePredicateRef(),
		SupervisorConfig: original, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := store.CreateTask(request.TaskID, &request, []byte("inspection brief"), &meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := supervisorcore.NewSavedBindingSource(store.RootID, infrastructure.SavedBindingTaskMetadata, request.TaskID, "", original)
	if err != nil {
		t.Fatal(err)
	}
	derived := original
	derived.ConfigDigest = task.ComputeSHA256([]byte("previously selected generation"))
	selection := supervisorcore.AdmissionSelection{SavedSupervisor: &derived, ContinuationSource: source}
	// The new task does not exist. Original metadata, not this task ID or the
	// derived binding, must own continuation recovery throughout admission.
	resolved, err := resolveAdmissionSelection(t.Context(), selection, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || resolved.ContinuationSource != source {
		t.Fatalf("continuation tried to read successor metadata: %v", err)
	}
	candidate := scopeInspectionCandidate(request)
	candidate.Inspection.Environment = []string{"TMPDIR=/selected-provider"}
	candidate.Inspection.Runtime.Environment = slices.Clone(candidate.Inspection.Environment)
	refused := errors.New("fixture stops at continuation owner")
	calls := 0
	routing := admissionContinuationRouting{prepare: func(_ context.Context, actual *taskdir.Store, selected infrastructure.LaunchContext, observed infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
		calls++
		assertAdmissionRoutingScope(t, selected, clientPath)
		if actual != store || observed != source {
			t.Fatal("continuation lost original metadata")
		}
		return infrastructure.Disposition{}, refused
	}}
	options := supervisorcore.AdmissionOptions{Routing: routing, InitialExecutable: clientPath, Control: supervisorcore.OptionsForEnvironment(pueue.Options{}, []string{"TMPDIR=/captured-caller"})}
	client, err := prepareAdmissionSupervisorContext(t.Context(), resolved, options, store, candidate, "")
	if client != nil || !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("derived candidate used strict Saved recovery or early scope: calls=%d err=%v", calls, err)
	}
	var staged *StageError
	if !errors.As(err, &staged) || staged.Stage() != "start-supervisor" {
		t.Fatal("continuation changed its public failure stage")
	}
}

func (admissionRoutingFunc) RecoverSaved(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	return infrastructure.Disposition{}, errors.New("unexpected saved route")
}

func (admissionRoutingFunc) PrepareContinuation(context.Context, *taskdir.Store, infrastructure.LaunchContext, infrastructure.SavedBindingSource) (infrastructure.Disposition, error) {
	return infrastructure.Disposition{}, errors.New("unexpected continuation routing")
}

func (admissionRoutingFunc) MigrateOwn(context.Context, *taskdir.Store, infrastructure.LaunchContext) (infrastructure.Disposition, infrastructure.ManualMigrationReport, error) {
	return infrastructure.Disposition{}, infrastructure.ManualMigrationReport{}, errors.New("unexpected manual route")
}

func TestAdmissionRoutingCapturesFinalCandidateControlScope(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	clientPath := writeAdmissionRoutingClient(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	candidate := scopeInspectionCandidate(request)
	candidate.Inspection.Environment = []string{"TMPDIR=/selected-provider", infrastructure.OriginEnvironment + "=old selected origin"}
	candidate.Inspection.Runtime.Environment = slices.Clone(candidate.Inspection.Environment)
	refused := errors.New("fixture ends at owner-routing boundary")
	calls := 0
	routing := admissionRoutingFunc(func(_ context.Context, actual *taskdir.Store, selected infrastructure.LaunchContext) (infrastructure.Disposition, error) {
		calls++
		assertAdmissionRoutingScope(t, selected, clientPath)
		if actual != store || selected.Root != store.Root || selected.RootID != store.RootID {
			t.Fatal("admission routing substituted its borrowed target")
		}
		origin, found, err := infrastructure.ReadOrigin(selected.ControlEnvironment)
		if err != nil || !found || origin.RootID() != store.RootID {
			t.Fatal("selected old origin became routing authority")
		}
		return infrastructure.Disposition{}, refused
	})
	options := supervisorcore.AdmissionOptions{Routing: routing, InitialExecutable: clientPath, Control: supervisorcore.OptionsForEnvironment(pueue.Options{}, []string{"TMPDIR=/captured-caller"})}
	client, err := prepareAdmissionSupervisorContext(t.Context(), supervisorcore.AdmissionSelection{}, options, store, candidate, "")
	if client != nil || !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("private candidate bypassed routed preparation: calls=%d err=%v", calls, err)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed owner route staged local supervisor: %v", err)
	}
}

func assertAdmissionRoutingScope(t *testing.T, selected infrastructure.LaunchContext, clientPath string) {
	t.Helper()
	if selected.SupervisorExecutable != clientPath {
		t.Fatal("routing substituted the selected installed client")
	}
	if !slices.Contains(selected.ControlEnvironment, "TMPDIR=/selected-provider") || !slices.Contains(selected.MaintenanceEnvironment, "TMPDIR=/captured-caller") {
		t.Fatal("routing captured before candidate merge or replaced maintenance scope")
	}
}

func writeAdmissionRoutingClient(t *testing.T) string {
	t.Helper()
	path := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueue")
	supervisorfixture.WriteExecutable(t, path, `#!/bin/sh
set -eu
case "${1-}" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  *) exit 64 ;;
esac
`)
	return supervisorfixture.CanonicalPath(t, path)
}
