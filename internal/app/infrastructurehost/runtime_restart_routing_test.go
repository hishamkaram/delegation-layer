package infrastructurehost

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// These retained completed records test real public readers/routing, not native
// births, current daemon readiness, or an execution outcome.
func seedRuntimeRoutingHistory(t *testing.T, store *taskdir.Store) (infrastructure.LaunchContext, infrastructure.RuntimeRestartReference, infrastructure.RuntimeRestartReference) {
	t.Helper()
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	selected := infrastructure.LaunchContext{
		Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS,
		ControlEnvironment: []string{}, MaintenanceEnvironment: []string{}, SupervisorExecutable: filepath.Join(store.Root, "pueue"),
	}
	contextRef, err := infrastructure.StageLaunchContext(t.Context(), store, selected)
	if err != nil {
		t.Fatal(err)
	}
	control, err := store.OpenInfrastructureContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	inputs := infrastructure.StartupInputs{
		Kind: infrastructure.StartupRootOwner, Root: store.Root, RootID: store.RootID, BootID: boot, RootDevice: pin.Device(), RootInode: pin.Inode(),
		Artifacts: infrastructure.Artifacts{Runner: canonicalTestExecutable(t)}, Context: contextRef,
	}
	first := infrastructure.StartupIntent{
		Protocol: infrastructure.RecordProtocolVersion, Sequence: 1, Epoch: strings.Repeat("a", 32), Inputs: inputs,
		AcceptedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}
	first.InputsSHA256, err = first.Inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	from, err := infrastructure.ReferenceStartup(first)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Sequence = 2
	second.Epoch = strings.Repeat("b", 32)
	second.Inputs.Initial = infrastructure.InitialOperation{Kind: infrastructure.InitialRuntimeRestart, RestartFrom: from}
	second.Inputs.RuntimeProducer = "outside"
	second.InputsSHA256, err = second.Inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	successor, err := infrastructure.ReferenceStartup(second)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []infrastructure.StartupIntent{first, second} {
		if _, err = control.PutContext(t.Context(), fmt.Sprintf("intent-%020d.json", record.Sequence), record); err != nil {
			t.Fatal(err)
		}
	}
	return selected, infrastructure.RuntimeRestartReference{RootID: store.RootID, Startup: from}, infrastructure.RuntimeRestartReference{RootID: store.RootID, Startup: successor}
}

func writeRuntimeReadyReceipt(t *testing.T, store *taskdir.Store, binding task.SupervisorRef, from, successor infrastructure.RuntimeRestartReference) {
	t.Helper()
	fromToken, err := infrastructure.FormatRuntimeRestartReference(from)
	if err != nil {
		t.Fatal(err)
	}
	successorToken, err := infrastructure.FormatRuntimeRestartReference(successor)
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(struct {
		Schema  int `json:"schema_version"`
		Runtime struct {
			From      string             `json:"from"`
			Binding   task.SupervisorRef `json:"binding"`
			Phase     string             `json:"phase"`
			OldPID    int                `json:"old_pid"`
			OldStart  string             `json:"old_process_start"`
			Successor string             `json:"successor"`
			NewPID    int                `json:"new_pid"`
			NewStart  string             `json:"new_process_start"`
		} `json:"runtime_restart"`
	}{Schema: task.SchemaVersion, Runtime: struct {
		From      string             `json:"from"`
		Binding   task.SupervisorRef `json:"binding"`
		Phase     string             `json:"phase"`
		OldPID    int                `json:"old_pid"`
		OldStart  string             `json:"old_process_start"`
		Successor string             `json:"successor"`
		NewPID    int                `json:"new_pid"`
		NewStart  string             `json:"new_process_start"`
	}{fromToken, binding, "ready", 101, "recorded-old", successorToken, 202, "recorded-new"}})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(store.Root, ".supervisor")
	if err = os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(base, "bootstrap.pending"), data)
}

func TestRuntimeRestartRoutingCompletedReceiptNeverStartsAgain(t *testing.T) {
	fixture := newInfrastructureSavedBindingFixture(t, infrastructure.SavedBindingTaskMetadata, true)
	selected, from, successor := seedRuntimeRoutingHistory(t, fixture.store)
	writeRuntimeReadyReceipt(t, fixture.store, fixture.binding, from, successor)
	unsetAppInvocationOrigin(t)
	routing := NewRouting(CaptureInvocation(t.Context()), "/must-not-resolve/delegate-run")
	before := supervisorfixture.SnapshotStore(t, fixture.store.Root)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 2 {
		disposition, report, err := routing.RestartOwn(ctx, fixture.store, selected, from)
		if err != nil || report.Phase != infrastructure.RuntimeRestartReady || report.From != from.Startup || report.Successor != successor.Startup || disposition != (infrastructure.Disposition{}) {
			t.Fatalf("completed operation was replayed or claimed current readiness: %+v %+v %v", disposition, report, err)
		}
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, fixture.store.Root)) {
		t.Fatal("completed receipt retry changed root or context history")
	}
}

func TestRuntimeRestartRoutingMissingReplacementRefusesBeforeContact(t *testing.T) {
	fixture := newInfrastructureSavedBindingFixture(t, infrastructure.SavedBindingTaskMetadata, true)
	selected, from, _ := seedRuntimeRoutingHistory(t, fixture.store)
	unsetAppInvocationOrigin(t)
	replacement := filepath.Join(fixture.store.Root, "missing-delegate-run")
	routing := NewRouting(CaptureInvocation(t.Context()), replacement)
	before := supervisorfixture.SnapshotStore(t, fixture.store.Root)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	disposition, report, err := routing.RestartOwn(ctx, fixture.store, selected, from)
	var missing *os.PathError
	if !errors.Is(err, os.ErrNotExist) || !errors.As(err, &missing) || missing.Path != replacement || disposition != (infrastructure.Disposition{}) || report != (infrastructure.RuntimeRestartReport{}) {
		t.Fatalf("missing replacement did not refuse before owner contact: %+v %+v %v", disposition, report, err)
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, fixture.store.Root)) {
		t.Fatal("missing replacement staged context, accepted restart or changed root")
	}
}

func TestRuntimeRestartRoutingMissingReferencePreservesRoot(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	selected := infrastructure.LaunchContext{Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS, ControlEnvironment: []string{}, MaintenanceEnvironment: []string{}, SupervisorExecutable: filepath.Join(store.Root, "pueue")}
	from := infrastructure.RuntimeRestartReference{RootID: store.RootID, Startup: infrastructure.StartupReference{Sequence: 1, Epoch: strings.Repeat("a", 32), IntentSHA256: strings.Repeat("b", 64)}}
	unsetAppInvocationOrigin(t)
	routing := NewRouting(CaptureInvocation(t.Context()), "/must-not-resolve/delegate-run")
	before := supervisorfixture.SnapshotStore(t, store.Root)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if disposition, report, err := routing.RestartOwn(ctx, store, selected, from); err == nil || disposition != (infrastructure.Disposition{}) || report != (infrastructure.RuntimeRestartReport{}) {
		t.Fatalf("missing startup became restart authority: %+v %+v %v", disposition, report, err)
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
		t.Fatal("refused reference staged context or initialized supervisor")
	}
	if _, err := os.Lstat(pueue.PrivateConfigPath(store.Root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing reference provisioned private config: %v", err)
	}
}
