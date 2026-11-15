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
