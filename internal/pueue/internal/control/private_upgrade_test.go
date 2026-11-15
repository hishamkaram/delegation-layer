package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This production-store regression protects the compatibility cutoff: a
// supported drain is readable by current code but never legacy startup authority.
func TestUpgradeDrainV1AcceptedWithoutLegacyBootstrapAuthority(t *testing.T) {
	fixture, _ := runtimeRestartJournalFixture(t)
	pin, err := readControlRootPin(t.Context(), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	type rootSubject struct {
		Path   string `json:"path"`
		ID     string `json:"id"`
		Device uint64 `json:"device"`
		Inode  uint64 `json:"inode"`
	}
	root := rootSubject{pin.Root(), pin.RootID(), pin.Device(), pin.Inode()}
	type drainSubject struct {
		Root        rootSubject        `json:"root"`
		Predecessor task.SupervisorRef `json:"predecessor"`
		OperationID string             `json:"operation_id"`
		Mode        string             `json:"mode"`
		Phase       string             `json:"phase"`
	}
	drain := drainSubject{root, fixture.client.Binding(), "upgrade-v1:11111111111111111111111111111111", "operation_required", "draining"}
	data, err := task.MarshalCanonical(struct {
		SchemaVersion int          `json:"schema_version"`
		Drain         drainSubject `json:"drain_v1"`
	}{task.SchemaVersion, drain})
	if err != nil {
		t.Fatal(err)
	}
	path := privateBootstrapRecordPath(fixture.root)
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, readErr := readPrivateBootstrapRecord(fixture.root); readErr != nil || !found {
		t.Fatalf("supported frozen drain-v1 was rejected: found=%t err=%v", found, readErr)
	}
	if pending, pendingErr := privateLegacyBootstrapPending(fixture.root); pendingErr != nil || pending {
		t.Fatalf("drain granted legacy bootstrap authority: pending=%t err=%v", pending, pendingErr)
	}
	if err = clearPrivateBootstrapPending(fixture.root); err != nil {
		t.Fatal(err)
	}
	if after := readPrivateSupervisorFile(t, path); !bytes.Equal(data, after) {
		t.Fatal("generic bootstrap clear changed the upgrade fence")
	}
}

func upgradeJournalFixture(t *testing.T, mode UpgradeMode) (generationGuardFixture, taskdir.RootPin, PrivateUpgradeObservation) {
	t.Helper()
	fixture, _ := runtimeRestartJournalFixture(t)
	pin, err := readControlRootPin(t.Context(), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	var accepted PrivateUpgradeObservation
	err = WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var beginErr error
		accepted, beginErr = BeginPrivateUpgradeWithLock(ctx, pin, mode, fixture.client.options)
		return beginErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, pin, accepted
}

func TestUpgradeDrainFencesGenericMutationWithoutErasingIntent(t *testing.T) {
	fixture, pin, accepted := upgradeJournalFixture(t, UpgradeOperationRequired)
	before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.root))
	if complete, err := PrivateSupervisorMigrationComplete(fixture.root, fixture.client.Binding()); complete || !errors.Is(err, ErrPrivateUpgradePending) {
		t.Fatalf("admission crossed drain: complete=%t err=%v", complete, err)
	}
	if _, err := fixture.client.checkSubmissionGate(t.Context(), nil); !errors.Is(err, ErrPrivateUpgradePending) {
		t.Fatalf("nil low-level gate crossed drain: %v", err)
	}
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		for _, operation := range []func() error{
			func() error { return markPrivateBootstrapPendingWithinCommit(ctx, fixture.root) },
			func() error { return fixture.client.StartPrivate(ctx, fixture.root) },
			func() error {
				return publishPrivateDaemonIdentityContext(ctx, fixture.base, fixture.client.Binding(), allowPrivateGenerationForTest{})
			},
			func() error {
				_, err := PreparePrivateWithLock(ctx, fixture.client.Binding().ClientExecutable, fixture.client.Binding().DaemonExecutable, fixture.root, fixture.client.options)
				return err
			},
			func() error { return fixture.client.MigratePrivateSupervisorWithLock(ctx, fixture.root) },
		} {
			if err := operation(); !errors.Is(err, ErrPrivateUpgradePending) {
				t.Errorf("generic mutation crossed drain: %v", err)
			}
		}
		return clearPrivateBootstrapPending(fixture.root)
	})
	if err != nil {
		t.Fatal(err)
	}
	observed, found, err := ObservePrivateUpgrade(t.Context(), pin, accepted.OperationID)
	if err != nil || !found || observed != accepted {
		t.Fatalf("drain winner changed: %+v found=%t err=%v", observed, found, err)
	}
	if !bytes.Equal(before, readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.root))) {
		t.Fatal("refused generic mutation changed drain")
	}
}

func TestUpgradeCompletionRequiresExactAwaitingOperation(t *testing.T) {
	fixture, pin, accepted := upgradeJournalFixture(t, UpgradeOperationRequired)
	wrong := UpgradeOperationID("upgrade-v1:22222222222222222222222222222222")
	before := snapshotPrivateSuccessorTestState(t, fixture.root)
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		if observed, err := AuthorizePrivateUpgradeCompletionWithLock(ctx, pin, accepted.OperationID, fixture.client.options); !errors.Is(err, ErrPrivateUpgradePending) || observed.Phase != UpgradeDraining {
			t.Fatalf("early complete authorized: %+v err=%v", observed, err)
		}
		if _, err := AuthorizePrivateUpgradeCompletionWithLock(ctx, pin, wrong, fixture.client.options); !errors.Is(err, ErrPrivateUpgradeOperation) {
			t.Fatalf("wrong OP accepted: %v", err)
		}
		selected := 0
		_, _, err := CompletePrivateUpgradeWithLock(ctx, pin, accepted.OperationID, func(context.Context) (string, string, error) { selected++; return "", "", ErrConfiguration }, fixture.client.options)
		if !errors.Is(err, ErrPrivateUpgradePending) || selected != 0 {
			t.Fatalf("early complete selected a pair: count=%d err=%v", selected, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateSuccessorTestStateUnchanged(t, fixture.root, before)
}

func TestUpgradeCompletionAuthorizationPrecedesSelection(t *testing.T) {
	fixture, pin, accepted := upgradeJournalFixture(t, UpgradeOperationRequired)
	var err error
	if err = os.WriteFile(filepath.Join(fixture.base, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.client.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error { return nil }
	err = WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		awaiting, advanceErr := AdvancePrivateUpgradeDrainWithLock(ctx, pin, accepted.OperationID, fixture.client.options)
		if advanceErr != nil || awaiting.Phase != UpgradeAwaitingInstallation {
			t.Fatalf("drain did not await installation: %+v err=%v", awaiting, advanceErr)
		}
		authorized, authErr := AuthorizePrivateUpgradeCompletionWithLock(ctx, pin, accepted.OperationID, fixture.client.options)
		if authErr != nil || authorized.Phase != UpgradeAuthorized {
			t.Fatalf("complete did not authorize: %+v err=%v", authorized, authErr)
		}
		record, found, readErr := readPrivateUpgrade(pin, accepted.OperationID)
		if readErr != nil || !found || record.CompletionAuthorization.OperationID != accepted.OperationID || record.SelectedPair != (privateUpgradePair{}) {
			t.Fatalf("authorization was not durable before selection: %+v found=%t err=%v", record, found, readErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeBeginConcurrentRequestsShareOneWinner(t *testing.T) {
	fixture, _ := runtimeRestartJournalFixture(t)
	pin, err := readControlRootPin(t.Context(), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type outcome struct {
		value PrivateUpgradeObservation
		err   error
	}
	results := make(chan outcome, 12)
	var joined sync.WaitGroup
	for range cap(results) {
		joined.Add(1)
		go func() {
			defer joined.Done()
			var value PrivateUpgradeObservation
			err := WithExistingBootstrapLockContext(ctx, fixture.root, func(locked context.Context) error {
				var err error
				value, err = BeginPrivateUpgradeWithLock(locked, pin, UpgradeOperationRequired, fixture.client.options)
				return err
			})
			results <- outcome{value, err}
		}()
	}
	joined.Wait()
	close(results)
	var winner PrivateUpgradeObservation
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if winner.OperationID == "" {
			winner = result.value
		}
		if result.value != winner {
			t.Fatalf("competing upgrade winner: got=%+v want=%+v", result.value, winner)
		}
	}
	if winner.OperationID == "" {
		t.Fatal("no committed operation returned")
	}
}

func assertPrivateSuccessorTestStateUnchanged(t *testing.T, root string, before map[string]privateSuccessorTestFile) {
	t.Helper()
	if !reflect.DeepEqual(before, snapshotPrivateSuccessorTestState(t, root)) {
		t.Fatal("refused upgrade changed durable state")
	}
}

func TestUpgradeDrainAllowsExactStashedCancelThroughInstalledTransport(t *testing.T) {
	f := newSavedPrivateBindingFixture(t, StateQueued)
	status := statusTestPayload(t, map[string]any{"7": statusTestJob(7, identityFromRecord(f.submission).Label(), map[string]any{"Stashed": map[string]any{"enqueue_at": nil}})}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	if err := os.WriteFile(f.statusPath, status, 0o600); err != nil {
		t.Fatal(err)
	}
	pin, err := readControlRootPin(t.Context(), f.private.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	options := f.options
	options.PrivateGenerationGuard = allowPrivateGenerationForTest{}
	err = WithExistingBootstrapLockContext(t.Context(), pin.Root(), func(ctx context.Context) error {
		_, beginErr := BeginPrivateUpgradeWithLock(ctx, pin, UpgradeOperationRequired, options)
		return beginErr
	})
	if err != nil {
		t.Fatal(err)
	}
	selectedClient, selectedDaemon := installPrivateSupervisorPair(t, filepath.Join(filepath.Dir(f.private.clientPath), "current"))
	if err = os.Remove(f.private.clientPath); err != nil {
		t.Fatal(err)
	}
	client, err := ProbeSavedPrivateUpgradeControl(t.Context(), pin.Root(), f.saved, options, func(context.Context) (string, string, error) { return selectedClient, selectedDaemon, nil })
	if err != nil || client == nil {
		t.Fatalf("drain predecessor control refused: client=%t err=%v", client != nil, err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	permit := createStopPermit(t, f.task)
	result, stopErr := client.Stop(t.Context(), permit)
	if releaseErr := permit.Release(); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if stopErr != nil || result.ObservedState != StateStashed || result.Action != "remove" || !result.Matched || !result.Attempted {
		t.Fatalf("exact stashed cancel lost authority: %+v err=%v", result, stopErr)
	}
	if _, gateErr := client.checkSubmissionGate(t.Context(), nil); !errors.Is(gateErr, ErrPrivateUpgradePending) {
		t.Fatalf("cancel transport gained admission authority: %v", gateErr)
	}
	assertPrivateDaemonStartCount(t, pin.Root(), 1)
}

func TestUpgradePreparePreservesExistingReactiveMode(t *testing.T) {
	f, pin, accepted := upgradeJournalFixture(t, UpgradeReactive)
	if err := os.WriteFile(filepath.Join(f.base, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.client.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error { return nil }
	err := WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		advanced, advanceErr := AdvancePrivateUpgradeDrainWithLock(ctx, pin, accepted.OperationID, f.client.options)
		if advanceErr != nil || advanced.Phase != UpgradeAuthorized {
			t.Fatalf("reactive drain did not authorize: %+v err=%v", advanced, advanceErr)
		}
		prepared, prepareErr := BeginPrivateUpgradeWithLock(ctx, pin, UpgradeOperationRequired, f.client.options)
		if prepareErr != nil || prepared != advanced {
			t.Fatalf("prepare converted reactive intent: %+v err=%v", prepared, prepareErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeBeginReportsOnlyCanonicalWinnerAcrossGuardFault(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "after_commit"}[commit], func(t *testing.T) {
			testUpgradeBeginGuardFault(t, commit)
		})
	}
}

func testUpgradeBeginGuardFault(t *testing.T, commit bool) {
	t.Helper()
	f, _ := runtimeRestartJournalFixture(t)
	pin, err := readControlRootPin(t.Context(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("fixture resident grant fault")
	options := f.client.options
	options.PrivateGenerationGuard = generationGuardFunc(func(_ context.Context, _ string, write func() error) error {
		if !commit {
			return fault
		}
		return errors.Join(write(), fault)
	})
	var observed PrivateUpgradeObservation
	err = WithExistingBootstrapLockContext(t.Context(), f.root, func(ctx context.Context) error {
		var beginErr error
		observed, beginErr = BeginPrivateUpgradeWithLock(ctx, pin, UpgradeOperationRequired, options)
		return beginErr
	})
	if !errors.Is(err, fault) {
		t.Fatalf("grant fault was lost: %v", err)
	}
	winner, found, readErr := ObservePrivateUpgrade(t.Context(), pin, "")
	if readErr != nil || found != commit {
		t.Fatalf("canonical cutoff differs: found=%t want=%t err=%v", found, commit, readErr)
	}
	if commit {
		if winner != observed || observed.OperationID == "" {
			t.Fatal("committed operation winner was hidden")
		}
	} else if observed.OperationID != "" {
		t.Fatal("pre-commit failure claimed an operation")
	}
}
