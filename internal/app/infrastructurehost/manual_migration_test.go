package infrastructurehost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// This finite effects fixture owns only call observations. Domain fixtures
// separately prove canonical record checks and the authenticated stop protocol.
type manualMigrationEffects struct {
	test         *testing.T
	context      context.Context
	artifacts    map[infrastructure.StartupReference]infrastructure.Artifacts
	readErr      error
	released     infrastructure.OwnerStopResult
	releaseErr   error
	replacement  infrastructure.EstablishedOwner
	establishErr error
	outcome      infrastructure.PreparationOutcome
	requestErr   error
	closureErr   error
	calls        []string
	requested    []infrastructure.EstablishedOwner
}

func (f *manualMigrationEffects) record(ctx context.Context, action string, owner infrastructure.StartupReference) {
	f.test.Helper()
	if ctx != f.context {
		f.test.Fatal("migration replaced the caller's shared control context")
	}
	f.calls = append(f.calls, fmt.Sprintf("%s:%d", action, owner.Sequence))
}

func (f *manualMigrationEffects) ReadArtifacts(ctx context.Context, owner infrastructure.StartupReference) (infrastructure.Artifacts, error) {
	f.record(ctx, "read", owner)
	if f.readErr != nil {
		return infrastructure.Artifacts{}, f.readErr
	}
	artifacts, found := f.artifacts[owner]
	if !found {
		return infrastructure.Artifacts{}, infrastructure.ErrIdentity
	}
	return artifacts, nil
}

func (f *manualMigrationEffects) ReleaseServing(ctx context.Context, owner infrastructure.StartupReference) (infrastructure.OwnerStopResult, error) {
	f.record(ctx, "release", owner)
	return f.released, f.releaseErr
}

func (f *manualMigrationEffects) EstablishReplacement(ctx context.Context) (infrastructure.EstablishedOwner, error) {
	f.record(ctx, "establish", infrastructure.StartupReference{})
	return f.replacement, f.establishErr
}

func (f *manualMigrationEffects) Request(ctx context.Context, owner infrastructure.EstablishedOwner) (infrastructure.PreparationOutcome, error) {
	f.record(ctx, "request", owner.Owner)
	f.requested = append(f.requested, owner)
	return f.outcome, f.requestErr
}

func (f *manualMigrationEffects) WaitProcessClosed(ctx context.Context, owner infrastructure.StartupReference) error {
	f.record(ctx, "wait", owner)
	return f.closureErr
}

func manualMigrationStartup(sequence uint64) infrastructure.StartupReference {
	return infrastructure.StartupReference{Sequence: sequence, Epoch: strings.Repeat("a", 32), IntentSHA256: strings.Repeat("b", 64)}
}

func newManualMigrationEffects(t *testing.T) (*manualMigrationEffects, infrastructure.EstablishedOwner) {
	t.Helper()
	a, b := manualMigrationStartup(1), manualMigrationStartup(2)
	return &manualMigrationEffects{
		test: t, context: t.Context(),
		artifacts: map[infrastructure.StartupReference]infrastructure.Artifacts{
			a: {Runner: filepath.Join(t.TempDir(), "missing-a")},
			b: {Runner: canonicalTestExecutable(t)},
		},
		released:    infrastructure.OwnerStopResult{Startup: a, State: infrastructure.OwnerServingReleased, ProcessClosure: infrastructure.OwnerProcessPending},
		replacement: infrastructure.EstablishedOwner{Owner: b, InitialResultFor: b},
		outcome: infrastructure.PreparationOutcome{Condition: infrastructure.PreparationOK, Report: infrastructure.ManualMigrationReport{
			State: infrastructure.ManualMigrationMigrated, Legacy: infrastructure.ManualMigrationLegacyRecovered, Active: infrastructure.ManualMigrationActiveNone,
		}},
	}, infrastructure.EstablishedOwner{Owner: a}
}

func TestManualMigrationPreparationContactsAtMostOneOwner(t *testing.T) {
	for _, test := range []struct {
		name        string
		setup       func(*manualMigrationEffects, *infrastructure.EstablishedOwner) infrastructure.InitialOperation
		calls       []string
		replacement bool
	}{
		{"healthy", func(f *manualMigrationEffects, owner *infrastructure.EstablishedOwner) infrastructure.InitialOperation {
			f.artifacts[owner.Owner] = infrastructure.Artifacts{Runner: canonicalTestExecutable(t)}
			return infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn}
		}, []string{"read:1", "request:1"}, false},
		{"newborn initial result", func(_ *manualMigrationEffects, owner *infrastructure.EstablishedOwner) infrastructure.InitialOperation {
			owner.InitialResultFor = owner.Owner
			return infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn}
		}, []string{"request:1"}, false},
		{"ensure replaces missing helper without waiting for daemon custody", func(f *manualMigrationEffects, _ *infrastructure.EstablishedOwner) infrastructure.InitialOperation {
			f.outcome.Report = infrastructure.ManualMigrationReport{}
			return infrastructure.InitialOperation{}
		}, []string{"read:1", "release:1", "establish:0", "request:2"}, true},
		{"explicit ensure replaces missing helper", func(f *manualMigrationEffects, _ *infrastructure.EstablishedOwner) infrastructure.InitialOperation {
			f.outcome.Report = infrastructure.ManualMigrationReport{}
			return infrastructure.InitialOperation{Kind: infrastructure.InitialEnsureOwn}
		}, []string{"read:1", "release:1", "establish:0", "request:2"}, true},
		{"recovery unchanged", func(f *manualMigrationEffects, _ *infrastructure.EstablishedOwner) infrastructure.InitialOperation {
			f.outcome.Report = infrastructure.ManualMigrationReport{}
			return infrastructure.InitialOperation{Kind: infrastructure.InitialRecoverSaved, Source: infrastructure.SavedBindingSource{
				RootID: strings.Repeat("c", 32), Kind: infrastructure.SavedBindingTaskMetadata, OwnerID: strings.Repeat("d", 32), BindingSHA256: strings.Repeat("e", 64),
			}}
		}, []string{"request:1"}, false},
		{"one replacement initial result", func(_ *manualMigrationEffects, _ *infrastructure.EstablishedOwner) infrastructure.InitialOperation {
			return infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn}
		}, []string{"read:1", "release:1", "establish:0", "request:2", "wait:1"}, true},
		{"concurrent healthy contact", func(f *manualMigrationEffects, _ *infrastructure.EstablishedOwner) infrastructure.InitialOperation {
			f.replacement.InitialResultFor = infrastructure.StartupReference{}
			return infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn}
		}, []string{"read:1", "release:1", "establish:0", "read:2", "request:2", "wait:1"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			effects, owner := newManualMigrationEffects(t)
			initial := test.setup(effects, &owner)
			outcome, err := requestInfrastructurePreparation(t.Context(), initial, owner, effects)
			if err != nil || outcome != effects.outcome || !slices.Equal(effects.calls, test.calls) || len(effects.requested) != 1 {
				t.Fatalf("migration contact changed: calls=%v outcome=%+v err=%v", effects.calls, outcome, err)
			}
			requested := owner
			if test.replacement {
				requested = effects.replacement
			}
			if effects.requested[0] != requested {
				t.Fatalf("initial result selector or exact request owner changed: %+v want %+v", effects.requested[0], requested)
			}
		})
	}
}

func TestManualMigrationPreparationRefusesBeforeWork(t *testing.T) {
	for _, operation := range []infrastructure.InitialOperationKind{infrastructure.InitialMigrateOwn, infrastructure.InitialEnsureOwn, ""} {
		t.Run(string(operation), func(t *testing.T) {
			for _, test := range []struct {
				name  string
				setup func(*manualMigrationEffects)
				want  error
				calls []string
			}{
				{"unreadable artifacts", func(f *manualMigrationEffects) { f.readErr = os.ErrPermission }, os.ErrPermission, []string{"read:1"}},
				{"missing artifacts are not missing helper", func(f *manualMigrationEffects) { f.readErr = os.ErrNotExist }, os.ErrNotExist, []string{"read:1"}},
				{"busy", func(f *manualMigrationEffects) { f.released.State = infrastructure.OwnerStopBusy }, infrastructure.ErrOwnerBusy, []string{"read:1", "release:1"}},
				{"unknown contact", func(f *manualMigrationEffects) { f.releaseErr = infrastructure.ErrOwnerRequest }, infrastructure.ErrOwnerRequest, []string{"read:1", "release:1"}},
				{"concurrent release loser", func(f *manualMigrationEffects) { f.releaseErr = task.ErrLockBusy }, task.ErrLockBusy, []string{"read:1", "release:1"}},
				{"changed selected owner", func(f *manualMigrationEffects) { f.released.Startup = f.replacement.Owner }, infrastructure.ErrIdentity, []string{"read:1", "release:1"}},
				{"birth fault", func(f *manualMigrationEffects) { f.establishErr = os.ErrPermission }, os.ErrPermission, []string{"read:1", "release:1", "establish:0"}},
				{"concurrent missing owner", func(f *manualMigrationEffects) {
					f.replacement.InitialResultFor = infrastructure.StartupReference{}
					f.artifacts[f.replacement.Owner] = infrastructure.Artifacts{Runner: filepath.Join(t.TempDir(), "missing-o2")}
				}, os.ErrNotExist, []string{"read:1", "release:1", "establish:0", "read:2"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					effects, owner := newManualMigrationEffects(t)
					test.setup(effects)
					_, err := requestInfrastructurePreparation(t.Context(), infrastructure.InitialOperation{Kind: operation}, owner, effects)
					if !errors.Is(err, test.want) || !slices.Equal(effects.calls, test.calls) || len(effects.requested) != 0 {
						t.Fatalf("refusal delivered or repeated work: calls=%v err=%v", effects.calls, err)
					}
				})
			}
		})
	}
}

func TestManualMigrationPreparationWaitsOnlyForMigratedReport(t *testing.T) {
	for _, test := range []struct {
		report infrastructure.ManualMigrationReport
		wait   bool
	}{
		{infrastructure.ManualMigrationReport{State: infrastructure.ManualMigrationBlocked, Legacy: infrastructure.ManualMigrationLegacyUnknown, Active: infrastructure.ManualMigrationActivePresent}, false},
		{infrastructure.ManualMigrationReport{State: infrastructure.ManualMigrationUnavailable, Legacy: infrastructure.ManualMigrationLegacyUnknown, Active: infrastructure.ManualMigrationActiveUnknown}, false},
		{infrastructure.ManualMigrationReport{State: infrastructure.ManualMigrationMigrated, Legacy: infrastructure.ManualMigrationLegacyRecovered, Active: infrastructure.ManualMigrationActiveNone}, true},
	} {
		t.Run(string(test.report.State), func(t *testing.T) {
			effects, owner := newManualMigrationEffects(t)
			effects.outcome.Condition = infrastructure.PreparationFault
			effects.outcome.Report = test.report
			effects.requestErr = os.ErrPermission
			effects.closureErr = context.DeadlineExceeded
			calls := []string{"read:1", "release:1", "establish:0", "request:2"}
			if test.wait {
				calls = append(calls, "wait:1")
			}
			outcome, err := requestInfrastructurePreparation(t.Context(), infrastructure.InitialOperation{Kind: infrastructure.InitialMigrateOwn}, owner, effects)
			if outcome != effects.outcome || !errors.Is(err, os.ErrPermission) || !slices.Equal(effects.calls, calls) || len(effects.requested) != 1 {
				t.Fatalf("migration error replayed work or lost report: calls=%v outcome=%+v err=%v", effects.calls, outcome, err)
			}
			if test.wait {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("original A closure failure was lost: %v", err)
				}
			} else if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("refusal waited for original A while D1 may still be live")
			}
		})
	}
}

func TestManualMigrationReportSurvivesLateCleanupFault(t *testing.T) {
	report := infrastructure.ManualMigrationReport{
		State: infrastructure.ManualMigrationMigrated, Legacy: infrastructure.ManualMigrationLegacyRecovered, Active: infrastructure.ManualMigrationActiveNone,
	}
	outcome := infrastructure.PreparationOutcome{
		Condition: infrastructure.PreparationFault, Report: report,
		Failure: (Host{}).ClassifyPreparation(os.ErrPermission),
	}
	if err := outcome.Validate(); err != nil {
		t.Fatalf("late cleanup fault invalidated completed report: %v", err)
	}
	if err := infrastructurePreparationOutcomeError(outcome); !errors.Is(err, infrastructure.ErrOwnerRequest) || outcome.Report != report {
		t.Fatalf("late cleanup lost completed report or failure: %v", err)
	}
}
