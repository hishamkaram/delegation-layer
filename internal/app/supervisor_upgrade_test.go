package app

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSupervisorUpgradeArgumentsAndCompletionArgv(t *testing.T) {
	op := "upgrade-v1:" + strings.Repeat("a", 32)
	for _, argv := range [][]string{
		{"supervisor", "ensure-owner", "--prepare-upgrade", "--json"},
		{"supervisor", "ensure-owner", "--complete-upgrade", op, "--refresh-owner", "--json"},
		{"supervisor", "ensure-owner", "--refresh-owner", "--json"},
	} {
		if _, err := ParseArguments(argv); err != nil {
			t.Fatalf("valid explicit upgrade request %q: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"supervisor", "ensure-owner", "--prepare-upgrade", "--complete-upgrade", op},
		{"supervisor", "ensure-owner", "--complete-upgrade", "arbitrary"},
		{"supervisor", "ensure-owner", "--complete-upgrade", ""},
		{"supervisor", "status", "--prepare-upgrade"},
		{"supervisor", "migrate", "--refresh-owner"},
	} {
		if _, err := ParseArguments(argv); err == nil {
			t.Fatalf("invalid upgrade argv accepted: %q", argv)
		}
	}
	for _, root := range []string{"", filepath.Join(t.TempDir(), "root with spaces")} {
		argv := supervisorUpgradeCompletionCommand(Arguments{Root: root}, op)
		parsed, err := ParseArguments(argv[1:])
		if err != nil || parsed.Root != root || parsed.SupervisorCompleteUpgrade != op || !parsed.SupervisorRefreshOwner || !parsed.JSON {
			t.Fatalf("completion argv changed subject: %q parsed=%+v err=%v", argv, parsed, err)
		}
		if root == "" && strings.Contains(strings.Join(argv, " "), "--root") {
			t.Fatal("default-root command fabricated a root argument")
		}
	}
}

func TestSupervisorUpgradeDefaultBootstrapRejectsOrphansAndHistoryBeforeEffects(t *testing.T) {
	for _, name := range []string{"orphan daemon", "corrupt history"} {
		t.Run(name, func(t *testing.T) {
			store := supervisorfixture.NewInspectionStore(t)
			writeBrokenSupervisorUpgradeFixture(t, store.Root, name)
			probe := filepath.Join(store.Root, ".probe")
			if err := os.Remove(probe); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(probe, []byte("filesystem probe forbidden"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := supervisorfixture.SnapshotStore(t, store.Root)
			result := supervisorEnsureOwner(Arguments{Root: store.Root}, Dependencies{})
			if result.code != 1 || result.response.SupervisorControl.Upgrade != nil {
				t.Fatalf("broken root treated as fresh: %+v", result)
			}
			if !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
				t.Fatal("orphan refusal probed, rotated or changed state")
			}
		})
	}
}

func TestSupervisorUpgradeDefaultBootstrapPreservesInitialIntent(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	if eligible, err := supervisorBootstrapAbsent(store.Root); err != nil || !eligible {
		t.Fatalf("fresh root lost bootstrap: %t %v", eligible, err)
	}
	base := filepath.Join(store.Root, ".supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "bootstrap.pending"), []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if eligible, err := supervisorBootstrapAbsent(store.Root); err != nil || !eligible {
		t.Fatalf("initial intent lost recovery: %t %v", eligible, err)
	}
}

func TestSupervisorUpgradeRefusesMissingRootWithoutCreatingIt(t *testing.T) {
	for _, arguments := range []Arguments{
		{SupervisorPrepareUpgrade: true},
		{SupervisorCompleteUpgrade: "upgrade-v1:" + strings.Repeat("a", 32)},
		{SupervisorRefreshOwner: true},
	} {
		arguments.Root = filepath.Join(t.TempDir(), "absent")
		result := supervisorEnsureOwner(arguments, Dependencies{})
		if result.code != 1 || result.err == nil {
			t.Fatalf("missing existing root accepted: %+v", result)
		}
		if _, err := os.Lstat(arguments.Root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused command created state: %v", err)
		}
	}
}

func TestSupervisorUpgradeProjectionSeparatesHistoryFromReadiness(t *testing.T) {
	binding := task.SupervisorRef{ClientExecutable: "/private/pueue", DaemonExecutable: "/private/pueued", ConfigPath: "/private/state/.supervisor/pueue.yml", ConfigDigest: strings.Repeat("b", 64), ResolvedConfigSHA256: strings.Repeat("c", 64), Endpoint: "unix:/private/socket", ObservedVersion: "pueue 4.0.4", ExecutableIdentity: task.ExecutableIdentityInstalledPathV1}
	outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradeOK, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}, Report: infrastructure.UpgradeReport{OperationID: infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32)), Mode: infrastructure.UpgradeOperationRequired, Phase: infrastructure.UpgradeMigrating, Predecessor: binding, Completed: true}}
	result := finishSupervisorUpgrade(Arguments{}, newSupervisorCommandResponse("ensure-owner"), outcome, nil)
	if result.code != 0 || result.response.Status == "owner_ready" || result.response.SupervisorControl.NextAction != "inspect_supervisor_status" {
		t.Fatalf("historical completion became admission authority: %+v", result)
	}
	data, err := json.Marshal(result.response.SupervisorControl)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "/private") || strings.Contains(string(data), "predecessor") {
		t.Fatalf("public control leaked internal binding: %s", data)
	}
	for _, cause := range []error{os.ErrPermission, errors.Join(pueue.ErrPrivateUpgradePending, os.ErrPermission), context.Canceled} {
		fault := finishSupervisorUpgrade(Arguments{}, newSupervisorCommandResponse("ensure-owner"), outcome, cause)
		if fault.code != 1 || fault.response.SupervisorControl.Upgrade == nil || !fault.response.SupervisorControl.Upgrade.Completed {
			t.Fatalf("fault lost committed progress: %+v", fault)
		}
	}
	outcome.Report = infrastructure.UpgradeReport{}
	outcome.Condition = infrastructure.UpgradePending
	outcome.ReasonCode = "ordinary_migration_accepted_pending"
	blocked := finishSupervisorUpgrade(Arguments{}, newSupervisorCommandResponse("ensure-owner"), outcome, nil)
	if blocked.code != 3 || blocked.response.SupervisorControl.NextAction != "run_supervisor_migrate" || blocked.response.SupervisorControl.Upgrade != nil {
		t.Fatalf("ordinary migration was adopted or looped: %+v", blocked)
	}
}

func TestSupervisorUpgradePrepareProjectionKeepsExactOperation(t *testing.T) {
	outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradeOK, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}, Report: infrastructure.UpgradeReport{OperationID: infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32)), Mode: infrastructure.UpgradeOperationRequired, Phase: infrastructure.UpgradeAwaitingInstallation, Predecessor: task.SupervisorRef{ClientExecutable: "/pueue", DaemonExecutable: "/pueued", ConfigPath: "/state/.supervisor/pueue.yml", ConfigDigest: strings.Repeat("b", 64), ResolvedConfigSHA256: strings.Repeat("c", 64), Endpoint: "unix:/socket", ObservedVersion: "pueue 4.0.4", ExecutableIdentity: task.ExecutableIdentityInstalledPathV1}}}
	a := Arguments{SupervisorPrepareUpgrade: true, Root: "/state"}
	prepared := finishSupervisorUpgrade(a, newSupervisorCommandResponse("ensure-owner"), outcome, nil)
	want := []string{"delegate", "--root", "/state", "supervisor", "ensure-owner", "--complete-upgrade", string(outcome.Report.OperationID), "--refresh-owner", "--json"}
	if prepared.code != 0 || !reflect.DeepEqual(prepared.response.SupervisorControl.Upgrade.CompletionCommand, want) {
		t.Fatalf("preparation result=%+v", prepared)
	}
	a.SupervisorPrepareUpgrade = false
	plain := finishSupervisorUpgrade(a, newSupervisorCommandResponse("ensure-owner"), outcome, nil)
	if plain.code != 3 || plain.response.SupervisorControl.Upgrade.OperationID != string(outcome.Report.OperationID) {
		t.Fatal("plain ensure authorized proactive completion")
	}
}

func TestSupervisorUpgradeFailureDiagnosticsPreserveTypedCondition(t *testing.T) {
	for _, fixture := range []struct {
		condition infrastructure.UpgradeCondition
		message   string
	}{
		{infrastructure.UpgradeRefused, "supervisor upgrade was refused"},
		{infrastructure.UpgradeUnsupported, "supervisor upgrade is unsupported"},
		{infrastructure.UpgradeFault, "supervisor upgrade failed"},
		{infrastructure.UpgradeTimeout, "supervisor upgrade timed out"},
	} {
		t.Run(string(fixture.condition), func(t *testing.T) {
			outcome := infrastructure.UpgradeOutcome{Condition: fixture.condition, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}}
			result := finishSupervisorUpgrade(Arguments{}, newSupervisorCommandResponse("ensure-owner"), outcome, nil)
			if result.code != 1 || result.response.Error != fixture.message || result.err == nil || result.err.Error() != fixture.message {
				t.Fatalf("typed failure was misreported: code=%d error=%q cause=%v", result.code, result.response.Error, result.err)
			}
			if result.response.SupervisorControl.NextAction != "stop_and_report" {
				t.Fatal("failed upgrade advised continued mutation")
			}
		})
	}
}

func TestSupervisorEnsureOwnerDrainingDiagnosticPreservesFailureGuidance(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	for _, scenario := range []struct {
		name      string
		condition infrastructure.UpgradeCondition
		cause     error
		code      int
		action    string
	}{
		{"pending", infrastructure.UpgradePending, nil, 3, "wait_for_legacy_work"},
		{"busy", infrastructure.UpgradeBusy, nil, 3, "wait_for_legacy_work"},
		{"fault", infrastructure.UpgradeFault, nil, 1, "stop_and_report"},
		{"refused", infrastructure.UpgradeRefused, nil, 1, "stop_and_report"},
		{"timeout", infrastructure.UpgradeTimeout, nil, 1, "stop_and_report"},
		{"pending with cleanup fault", infrastructure.UpgradePending, errors.Join(pueue.ErrPrivateUpgradePending, os.ErrPermission), 1, "stop_and_report"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			routing := &supervisorUpgradeReplyRouting{
				componentPreparationRouting: componentPreparationRouting{test: t, owner: fixture.component, client: fixture.clientB, options: fixture.options},
				outcome: infrastructure.UpgradeOutcome{Condition: scenario.condition, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}, Report: infrastructure.UpgradeReport{
					OperationID: infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32)), Mode: infrastructure.UpgradeReactive, Phase: infrastructure.UpgradeDraining, Predecessor: fixture.predecessor,
				}},
				cause: scenario.cause,
			}
			deps := fixture.dependencies()
			deps.ownerRouting = routing
			result := supervisorEnsureOwner(Arguments{Root: fixture.root}, deps)
			control := result.response.SupervisorControl
			if routing.calls != 1 || result.code != scenario.code || control.NextAction != scenario.action {
				t.Fatalf("draining diagnostic changed original recovery action: calls=%d code=%d control=%+v err=%v cause=%v", routing.calls, result.code, control, result.err, errors.Unwrap(result.err))
			}
			if control.DiagnosticSnapshotAt == "" || control.ActiveWork != "present" || control.MigrationReadiness != "blocked" || control.FirstBlocker == nil || control.FirstBlocker.NextAction != "wait_for_legacy_work" {
				t.Fatalf("original failure suppressed the later running-task diagnostic: %+v", control)
			}
			if scenario.cause != nil && (!errors.Is(result.err, os.ErrPermission) || !errors.Is(result.err, pueue.ErrPrivateUpgradePending)) {
				t.Fatalf("diagnostic lost original cleanup fault: %v", result.err)
			}
		})
	}
}

func TestSupervisorEnsureOwnerDrainingDiagnosticFailureRetainsBothCauses(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	status := readSupervisorProbeTestFile(t, fixture.statusPath)
	t.Cleanup(func() {
		if err := os.WriteFile(fixture.statusPath, status, 0o600); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(fixture.statusPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{nil, os.ErrPermission} {
		routing := &supervisorUpgradeReplyRouting{
			componentPreparationRouting: componentPreparationRouting{test: t, owner: fixture.component, client: fixture.clientB, options: fixture.options},
			outcome: infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradePending, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}, Report: infrastructure.UpgradeReport{
				OperationID: infrastructure.UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32)), Mode: infrastructure.UpgradeReactive, Phase: infrastructure.UpgradeDraining, Predecessor: fixture.predecessor,
			}},
			cause: cause,
		}
		deps := fixture.dependencies()
		deps.ownerRouting = routing
		result := supervisorEnsureOwner(Arguments{Root: fixture.root}, deps)
		if routing.calls != 1 || result.code != 1 || result.response.SupervisorControl.NextAction != "stop_and_report" {
			t.Fatalf("failed diagnostic changed fail-stop guidance: %+v err=%v", result.response.SupervisorControl, result.err)
		}
		if !errors.Is(result.err, pueue.ErrUnknown) || cause != nil && !errors.Is(result.err, cause) {
			t.Fatalf("diagnostic lost original or later observation fault: %v", result.err)
		}
	}
}

func TestSupervisorUpgradeStatusDiagnosticFailureStopsFurtherMaintenance(t *testing.T) {
	fixture := prepareHistoricalObservationPhase(t, pueue.StateRunning)
	supervisorfixture.WriteUpgradeDrain(t, fixture.store, fixture.predecessor)
	healthy := supervisorStatus(fixture.root, fixture.dependencies(), newSupervisorCommandResponse("status"))
	control := healthy.response.SupervisorControl
	if healthy.code != 0 || healthy.err != nil || control.ActiveWork != "present" || control.NextAction != "wait_for_legacy_work" {
		t.Fatalf("healthy draining status lost its observation semantics: %+v err=%v", control, healthy.err)
	}
	status := readSupervisorProbeTestFile(t, fixture.statusPath)
	t.Cleanup(func() {
		if err := os.WriteFile(fixture.statusPath, status, 0o600); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(fixture.statusPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := supervisorStatus(fixture.root, fixture.dependencies(), newSupervisorCommandResponse("status"))
	control = failed.response.SupervisorControl
	if failed.code != 1 || !errors.Is(failed.err, pueue.ErrBinding) || control.NextAction != "stop_and_report" || control.DiagnosticSnapshotAt == "" {
		t.Fatalf("failed draining status invited more maintenance or lost its diagnostic: code=%d control=%+v err=%v", failed.code, control, failed.err)
	}
}

type supervisorUpgradeReplyRouting struct {
	componentPreparationRouting
	outcome infrastructure.UpgradeOutcome
	cause   error
	calls   int
}

func (r *supervisorUpgradeReplyRouting) UpgradeOwn(ctx context.Context, store *taskdir.Store, selected infrastructure.LaunchContext, request infrastructure.UpgradeRequest, _ bool) (infrastructure.UpgradeOutcome, error) {
	if err := ctx.Err(); err != nil {
		return infrastructure.UpgradeOutcome{}, err
	}
	if selected.Root != store.Root || selected.RootID != store.RootID || request.Action != infrastructure.UpgradeEnsure {
		return infrastructure.UpgradeOutcome{}, infrastructure.ErrIdentity
	}
	r.calls++
	return r.outcome, r.cause
}

func writeBrokenSupervisorUpgradeFixture(t *testing.T, root, name string) {
	t.Helper()
	base := filepath.Join(root, ".supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	path, data := filepath.Join(base, "migration-history.json"), []byte("{broken")
	if name == "orphan daemon" {
		path = filepath.Join(base, "daemon.identity.json")
		data = []byte(`{"executable":"/installed/pueued","executable_identity":"installed-path-v1"}`)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
