package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The real private store is prepared without starting a daemon. Recorded
// process identities exercise journal validation, not native closure claims.
func runtimeRestartJournalFixture(t *testing.T) (generationGuardFixture, RuntimeRestartReference) {
	t.Helper()
	fixture := newGenerationGuardFixture(t)
	fixture.client.options.ControlOwnerExecutable = os.Args[0]
	if err := publishPrivateSupervisorBindingIfAbsentContext(t.Context(), fixture.root, fixture.client.Binding(), allowPrivateGenerationForTest{}); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateDaemonIdentityContext(t.Context(), fixture.base, fixture.client.Binding(), allowPrivateGenerationForTest{}); err != nil {
		t.Fatal(err)
	}
	from := runtimeRestartTestReference(t, fixture.root, 1)
	fixture.client.options.PrivateRuntimeRestartAuthorizer = func(_ context.Context, root string, reference RuntimeRestartReference, binding task.SupervisorRef, _ []Job) error {
		if root != fixture.root || reference != from || binding != fixture.client.Binding() {
			return ErrPrivateRuntimeRestartAuthorization
		}
		return nil
	}
	return fixture, from
}

func runtimeRestartTestReference(t *testing.T, root string, sequence int) RuntimeRestartReference {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	var identity task.RootRecord
	if err = task.DecodeStrict(data, &identity); err != nil {
		t.Fatal(err)
	}
	return RuntimeRestartReference(fmt.Sprintf("v1:%s:%d:%032x:%s", identity.RootID, sequence, sequence, strings.Repeat("a", 64)))
}

func acceptRuntimeRestartTest(t *testing.T, fixture generationGuardFixture, from RuntimeRestartReference) privateRuntimeRestartRecord {
	t.Helper()
	var accepted privateRuntimeRestartRecord
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		var err error
		accepted, err = fixture.client.acceptPrivateRuntimeRestart(ctx, fixture.root, from, privateProcessIdentity{PID: 101, StartToken: "recorded-old"}, nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return accepted
}

func TestRuntimeRestartJournalFencesGenericConsumers(t *testing.T) {
	fixture, from := runtimeRestartJournalFixture(t)
	acceptRuntimeRestartTest(t, fixture, from)
	path := privateBootstrapRecordPath(fixture.root)
	before := readPrivateSupervisorFile(t, path)
	observation, observationErr := ObservePrivateRuntimeRestart(t.Context(), fixture.root, from)
	if observationErr != nil || observation.From != from || observation.Phase != RuntimeRestartRequested || observation.Binding != fixture.client.Binding() {
		t.Fatalf("accepted runtime observation = %+v, %v", observation, observationErr)
	}
	assertRuntimeRestartFenceKinds(t, fixture)
	if err := clearPrivateBootstrapPending(fixture.root); err != nil {
		t.Fatal(err)
	}
	if after := readPrivateSupervisorFile(t, path); !bytes.Equal(before, after) {
		t.Fatal("generic clear removed runtime intent")
	}
	assertRuntimeRestartGenericMutationFences(t, fixture)
	assertRuntimeRestartAdmissionFences(t, fixture)
	if after := readPrivateSupervisorFile(t, path); !bytes.Equal(before, after) {
		t.Fatal("refused generic operations changed runtime intent")
	}
}

func assertRuntimeRestartFenceKinds(t *testing.T, fixture generationGuardFixture) {
	t.Helper()
	if pending, err := PrivateRuntimeRestartPending(t.Context(), fixture.root); err != nil || !pending {
		t.Fatalf("runtime fence = %t, %v", pending, err)
	}
	if pending, err := privateLegacyBootstrapPending(fixture.root); err != nil || pending {
		t.Fatalf("runtime fence granted legacy repair = %t, %v", pending, err)
	}
}

func assertRuntimeRestartGenericMutationFences(t *testing.T, fixture generationGuardFixture) {
	t.Helper()
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		operations := []func() error{
			func() error { return markPrivateBootstrapPendingWithinCommit(ctx, fixture.root) },
			func() error { return fixture.client.StartPrivate(ctx, fixture.root) },
			func() error {
				return publishPrivateDaemonIdentityContext(ctx, fixture.base, fixture.client.Binding(), allowPrivateGenerationForTest{})
			},
			func() error {
				_, reconciliationErr := ReconcilePublishedPrivatePairAttemptWithLock(ctx, fixture.root, func(context.Context) (string, string, error) {
					t.Error("runtime fence reached replacement selection")
					return "", "", ErrConfiguration
				}, Options{})
				return reconciliationErr
			},
		}
		for _, operation := range operations {
			if operationErr := operation(); !errors.Is(operationErr, ErrPrivateRuntimeRestartPending) {
				return fmt.Errorf("runtime fence bypassed: %w", operationErr)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertRuntimeRestartAdmissionFences(t *testing.T, fixture generationGuardFixture) {
	t.Helper()
	if err := CheckPrivateAdmissionRuntime(t.Context(), fixture.root, fixture.client.Binding()); !errors.Is(err, ErrPrivateRuntimeRestartPending) || errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("early runtime admission = %v", err)
	}
	if complete, err := PrivateSupervisorMigrationComplete(fixture.root, fixture.client.Binding()); complete || !errors.Is(err, ErrPrivateRuntimeRestartPending) {
		t.Fatalf("final generation completion = %t, %v", complete, err)
	}
}

func TestRuntimeRestartAcceptanceRequiresLiveAuthority(t *testing.T) {
	denied := errors.New("restart owner refused")
	for _, fault := range []string{"missing-authorizer", "authorizer-refused", "missing-generation", "generation-refused", "missing-ex", "expired-ex"} {
		t.Run(fault, func(t *testing.T) {
			fixture, from := runtimeRestartJournalFixture(t)
			ctx := t.Context()
			expected := error(ErrBinding)
			switch fault {
			case "missing-authorizer":
				fixture.client.options.PrivateRuntimeRestartAuthorizer = nil
				expected = ErrPrivateRuntimeRestartAuthorization
			case "authorizer-refused":
				fixture.client.options.PrivateRuntimeRestartAuthorizer = func(context.Context, string, RuntimeRestartReference, task.SupervisorRef, []Job) error { return denied }
				expected = denied
			case "missing-generation":
				fixture.client.options.PrivateGenerationGuard = nil
				expected = ErrInfrastructureOwnerRequired
			case "generation-refused":
				fixture.client.options.PrivateGenerationGuard = generationGuardFunc(func(context.Context, string, func() error) error { return denied })
				expected = denied
			case "missing-ex":
			case "expired-ex":
				if err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(locked context.Context) error { ctx = locked; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			attempt := func(locked context.Context) error {
				_, err := fixture.client.acceptPrivateRuntimeRestart(locked, fixture.root, from, privateProcessIdentity{PID: 101, StartToken: "recorded-old"}, nil)
				return err
			}
			var err error
			if fault == "missing-ex" || fault == "expired-ex" {
				err = attempt(ctx)
			} else {
				err = WithExistingBootstrapLockContext(ctx, fixture.root, attempt)
			}
			if !errors.Is(err, expected) {
				t.Fatalf("unauthorized restart = %v, want %v", err, expected)
			}
			if _, err := os.Stat(privateBootstrapRecordPath(fixture.root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unauthorized restart wrote intent: %v", err)
			}
		})
	}
}

func TestRuntimeRestartAcceptancePreservesCompetingTransitions(t *testing.T) {
	for _, transition := range []string{"migration", "recovery", "identity-publication"} {
		t.Run(transition, func(t *testing.T) {
			fixture, from := runtimeRestartJournalFixture(t)
			installRuntimeRestartCompetingTransition(t, fixture, transition)
			before := snapshotPrivateSuccessorTestState(t, fixture.root)
			err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
				_, acceptanceErr := fixture.client.acceptPrivateRuntimeRestart(ctx, fixture.root, from, privateProcessIdentity{PID: 101, StartToken: "recorded-old"}, nil)
				return acceptanceErr
			})
			var classified *PrivateRuntimeRestartError
			if !errors.As(err, &classified) || classified.ReasonCode != "competing_transition" {
				t.Fatalf("competing %s was accepted: %v", transition, err)
			}
			if _, err := os.Stat(privateBootstrapRecordPath(fixture.root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("competing transition acquired runtime intent: %v", err)
			}
			if after := snapshotPrivateSuccessorTestState(t, fixture.root); !reflect.DeepEqual(before, after) {
				t.Fatal("refused runtime acceptance changed competing transition")
			}
		})
	}
}

func installRuntimeRestartCompetingTransition(t *testing.T, fixture generationGuardFixture, transition string) {
	t.Helper()
	switch transition {
	case "migration":
		if err := publishPrivateMigrationState(fixture.base, fixture.record); err != nil {
			t.Fatal(err)
		}
	case "recovery":
		if err := fixture.client.PersistPrivateRecoveryBindingContext(t.Context(), fixture.root); err != nil {
			t.Fatal(err)
		}
		record, found, err := readPrivateRecoveryRecord(fixture.root)
		if err != nil || !found {
			t.Fatalf("recovery fixture unavailable: found=%t err=%v", found, err)
		}
		data, err := task.MarshalCanonical(privateRecoveryPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: record})
		if err != nil {
			t.Fatal(err)
		}
		if err = publishPrivateRecord(fixture.base, privateRecoveryPendingRecordPath(fixture.root), data); err != nil {
			t.Fatal(err)
		}
	case "identity-publication":
		data, err := task.MarshalCanonical(privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: fixture.client.Binding()})
		if err != nil {
			t.Fatal(err)
		}
		if err = publishPrivateRecord(fixture.base, privateIdentityPublicationPath(fixture.root), data); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown competing transition %q", transition)
	}
}

func TestRuntimeRestartAcceptanceDistinguishesRetainedRecoveryAlias(t *testing.T) {
	for _, condition := range []string{"quiescent-current", "malformed", "foreign"} {
		t.Run(condition, func(t *testing.T) {
			fixture, from := runtimeRestartJournalFixture(t)
			if err := fixture.client.PersistPrivateRecoveryBindingContext(t.Context(), fixture.root); err != nil {
				t.Fatal(err)
			}
			mutateRuntimeRestartAliasFixture(t, fixture, condition)
			aliasBefore := readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.root))
			configBefore := readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.root))
			err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
				_, acceptanceErr := fixture.client.acceptPrivateRuntimeRestart(ctx, fixture.root, from, privateProcessIdentity{PID: 101, StartToken: "recorded-old"}, nil)
				return acceptanceErr
			})
			assertRuntimeRestartAliasAcceptance(t, fixture, from, condition, err)
			if !bytes.Equal(aliasBefore, readPrivateSupervisorFile(t, privateRecoveryRecordPath(fixture.root))) ||
				!bytes.Equal(configBefore, readPrivateSupervisorFile(t, privateRecoveryConfigPath(fixture.root))) {
				t.Fatal("runtime restart changed alias authority or bytes")
			}
		})
	}
}

func mutateRuntimeRestartAliasFixture(t *testing.T, fixture generationGuardFixture, condition string) {
	t.Helper()
	switch condition {
	case "quiescent-current":
	case "malformed":
		if err := os.WriteFile(privateRecoveryRecordPath(fixture.root), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "foreign":
		record, _, err := readPrivateRecoveryRecord(fixture.root)
		if err != nil {
			t.Fatal(err)
		}
		record.Supervisor.ConfigDigest = strings.Repeat("f", 64)
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(fixture.root), record)
	default:
		t.Fatalf("unknown recovery alias condition %q", condition)
	}
}

func assertRuntimeRestartAliasAcceptance(t *testing.T, fixture generationGuardFixture, from RuntimeRestartReference, condition string, acceptanceErr error) {
	t.Helper()
	if condition != "quiescent-current" {
		if acceptanceErr == nil {
			t.Fatal("unresolved alias accepted runtime restart")
		}
		if _, err := os.Stat(privateBootstrapRecordPath(fixture.root)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unresolved alias acquired runtime intent: %v", err)
		}
		return
	}
	if acceptanceErr != nil {
		t.Fatalf("cleanup-only alias prevented quiescent restart: %v", acceptanceErr)
	}
	if observed, err := ObservePrivateRuntimeRestart(t.Context(), fixture.root, from); err != nil || observed.Phase != RuntimeRestartRequested {
		t.Fatalf("cleanup-only alias restart observation: %+v, %v", observed, err)
	}
}

func runtimeRestartCompletedRecord(t *testing.T, fixture generationGuardFixture, from RuntimeRestartReference) privateRuntimeRestartRecord {
	t.Helper()
	return privateRuntimeRestartRecord{
		From: from, Binding: fixture.client.Binding(), Phase: RuntimeRestartReady,
		OldPID: 101, OldProcessStart: "recorded-old", Successor: runtimeRestartTestReference(t, fixture.root, 2),
		NewPID: 102, NewProcessStart: "recorded-new",
	}
}

func writeRuntimeRestartFixtureRecord(t *testing.T, root string, record privateBootstrapRecord) {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(privateBootstrapRecordPath(root), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRestartCompletedReceiptIsBoundedAndNotBootstrapAuthority(t *testing.T) {
	fixture, from := runtimeRestartJournalFixture(t)
	completed := runtimeRestartCompletedRecord(t, fixture, from)
	writeRuntimeRestartFixtureRecord(t, fixture.root, privateBootstrapRecord{SchemaVersion: task.SchemaVersion, RuntimeRestart: &completed})
	before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.root))
	if pending, err := PrivateBootstrapPending(fixture.root); err != nil || pending {
		t.Fatalf("completed receipt blocks ordinary generation: %t, %v", pending, err)
	}
	if pending, err := PrivateRuntimeRestartPending(t.Context(), fixture.root); err != nil || pending {
		t.Fatalf("completed receipt remains runtime pending: %t, %v", pending, err)
	}
	if err := clearPrivateBootstrapPending(fixture.root); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.root))) {
		t.Fatal("generic clear discarded bounded receipt")
	}
	if observed, err := ObservePrivateRuntimeRestart(t.Context(), fixture.root, from); err != nil || observed.Phase != RuntimeRestartReady {
		t.Fatalf("exact completed receipt unavailable: %+v, %v", observed, err)
	}
	next := completed.Successor
	fixture.client.options.PrivateRuntimeRestartAuthorizer = func(_ context.Context, _ string, got RuntimeRestartReference, _ task.SupervisorRef, _ []Job) error {
		if got != next {
			return ErrPrivateRuntimeRestartAuthorization
		}
		return nil
	}
	acceptRuntimeRestartTest(t, fixture, next)
	_, err := ObservePrivateRuntimeRestart(t.Context(), fixture.root, from)
	var classification *PrivateRuntimeRestartError
	if !errors.Is(err, ErrPrivateRuntimeRestartReferenceNotRetained) || !errors.As(err, &classification) || classification.ReasonCode != "restart_reference_not_retained" {
		t.Fatalf("superseded receipt asserted a result: %v", err)
	}
	if observed, err := ObservePrivateRuntimeRestart(t.Context(), fixture.root, next); err != nil || observed.Phase != RuntimeRestartRequested {
		t.Fatalf("qualified next restart was not accepted once: %+v, %v", observed, err)
	}
}

func TestRuntimeRestartCompletedReceiptSupersessionRequiresQualifiedBootstrap(t *testing.T) {
	fixture, from := runtimeRestartJournalFixture(t)
	completed := runtimeRestartCompletedRecord(t, fixture, from)
	writeRuntimeRestartFixtureRecord(t, fixture.root, privateBootstrapRecord{SchemaVersion: task.SchemaVersion, RuntimeRestart: &completed})
	if err := markPrivateBootstrapPendingWithinCommit(t.Context(), fixture.root); !errors.Is(err, ErrBinding) {
		t.Fatalf("unqualified bootstrap discarded receipt: %v", err)
	}
	denied := errors.New("resident revoked")
	err := WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		return guardPrivateGeneration(ctx, fixture.root, generationGuardFunc(func(context.Context, string, func() error) error { return denied }), func() error {
			return markPrivateBootstrapPendingWithinCommit(ctx, fixture.root)
		})
	})
	if !errors.Is(err, denied) {
		t.Fatalf("revoked generation discarded receipt: %v", err)
	}
	if observed, observationErr := ObservePrivateRuntimeRestart(t.Context(), fixture.root, from); observationErr != nil || observed.Phase != RuntimeRestartReady {
		t.Fatalf("denied supersession changed receipt: %+v, %v", observed, observationErr)
	}
	err = WithExistingBootstrapLockContext(t.Context(), fixture.root, func(ctx context.Context) error {
		return guardPrivateGeneration(ctx, fixture.root, allowPrivateGenerationForTest{}, func() error {
			return markPrivateBootstrapPendingWithinCommit(ctx, fixture.root)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := privateLegacyBootstrapPending(fixture.root); err != nil || !pending {
		t.Fatalf("qualified bootstrap was not published: %t, %v", pending, err)
	}
	if _, err := ObservePrivateRuntimeRestart(t.Context(), fixture.root, from); !errors.Is(err, ErrPrivateRuntimeRestartReferenceNotRetained) {
		t.Fatalf("discarded maintenance proof was treated as retained: %v", err)
	}
}

func TestRuntimeRestartJournalStrictValidationPreservesLegacyProtocol(t *testing.T) {
	fixture, from := runtimeRestartJournalFixture(t)
	valid := runtimeRestartCompletedRecord(t, fixture, from)
	for _, fault := range []string{"both-purposes", "unknown-phase", "same-successor", "missing-ready-process", "reused-incarnation", "foreign-binding"} {
		t.Run(fault, func(t *testing.T) {
			bootstrap := runtimeRestartRecordFault(t, fixture.root, valid, fault)
			writeRuntimeRestartFixtureRecord(t, fixture.root, bootstrap)
			if _, _, err := readPrivateBootstrapRecord(fixture.root); err == nil {
				t.Fatal("invalid runtime intent was accepted")
			}
			if _, err := PrivateRuntimeRestartPending(t.Context(), fixture.root); err == nil {
				t.Fatal("invalid runtime intent was treated as absence")
			}
		})
	}
	writeRuntimeRestartFixtureRecord(t, fixture.root, privateBootstrapRecord{SchemaVersion: task.SchemaVersion, RuntimeRestart: &valid})
	var legacy struct {
		SchemaVersion int                       `json:"schema_version"`
		Restart       *privatePairRestartIntent `json:"restart,omitempty"`
	}
	if err := task.DecodeStrict(readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.root)), &legacy); err == nil {
		t.Fatal("old strict reader accepted runtime-purpose record")
	}
	pair := privatePairRestartIntent{Predecessor: valid.Binding, Target: valid.Binding}
	if err := validatePrivatePairRestartIntent(fixture.root, pair); err == nil {
		t.Fatal("same-binding runtime support weakened unequal-pair validation")
	}
	pair.Target.DaemonExecutable += "-next"
	writeRuntimeRestartFixtureRecord(t, fixture.root, privateBootstrapRecord{SchemaVersion: task.SchemaVersion, Restart: &pair})
	if record, found, err := readPrivateBootstrapRecord(fixture.root); err != nil || !found || record.Restart == nil || record.RuntimeRestart != nil {
		t.Fatalf("legacy unequal pair stopped decoding: %+v, %t, %v", record, found, err)
	}
}

func runtimeRestartRecordFault(t *testing.T, root string, record privateRuntimeRestartRecord, fault string) privateBootstrapRecord {
	t.Helper()
	bootstrap := privateBootstrapRecord{SchemaVersion: task.SchemaVersion, RuntimeRestart: &record}
	switch fault {
	case "both-purposes":
		bootstrap.Restart = &privatePairRestartIntent{Predecessor: record.Binding, Target: record.Binding}
	case "unknown-phase":
		record.Phase = "other"
	case "same-successor":
		record.Successor = record.From
	case "missing-ready-process":
		record.NewPID, record.NewProcessStart = 0, ""
	case "reused-incarnation":
		record.NewPID, record.NewProcessStart = record.OldPID, record.OldProcessStart
	case "foreign-binding":
		record.Binding.ConfigPath = filepath.Join(root, "other", "pueue.yml")
	default:
		t.Fatalf("unknown runtime record fault %q", fault)
	}
	return bootstrap
}

func TestRuntimeRestartObservationNeverCreatesState(t *testing.T) {
	missing := filepath.Join(canonicalTemp(t), "missing")
	from := RuntimeRestartReference("v1:missing-reference")
	if pending, err := PrivateRuntimeRestartPending(t.Context(), missing); err != nil || pending {
		t.Fatalf("absent runtime observation = %t, %v", pending, err)
	}
	if _, err := ObservePrivateRuntimeRestart(t.Context(), missing, from); !errors.Is(err, ErrPrivateRuntimeRestartReferenceNotRetained) {
		t.Fatalf("absent receipt observation = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := PrivateRuntimeRestartPending(ctx, missing); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled observation ignored deadline: %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("observational command created root: %v", err)
	}
}
