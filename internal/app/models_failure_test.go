package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

type modelsFailureCase struct {
	name       string
	args       func(root, cwd string) []string
	deps       func(t *testing.T, cwd string) Dependencies
	wantExit   int
	wantStatus string
	wantReason string
	wantFail   DispatchFailureResponse
	wantError  string
	// forbidden holds raw cause text that no public output may contain.
	forbidden []string
}

func modelsFixtureArgs(root, cwd string) []string {
	return []string{"--root", root, "models", "--provider", config.ProviderFixture, "--cwd", cwd}
}

func modelsFixtureDeps(prepareErr error) func(*testing.T, string) Dependencies {
	return func(t *testing.T, cwd string) Dependencies {
		return Dependencies{Catalog: modelsFailureCatalog(t, cwd, prepareErr)}
	}
}

const modelsFailurePrivatePath = "/private/user/state/.supervisor/pueue.yml"

// modelsFailureLeaky wraps a cause with text no public output may contain.
func modelsFailureLeaky(cause error) error {
	return fmt.Errorf("credential %s at %s: %w", modelsFailureCanary, modelsFailurePrivatePath, cause)
}

func runModelsFailureCases(t *testing.T, cases []modelsFailureCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := testsupport.CanonicalTempDir(t)
			cwd := testsupport.CanonicalTempDir(t)
			assertModelsFailure(t, test, test.args(root, cwd), test.deps(t, cwd))
		})
	}
}

func TestModelsOperationalFailureExposesTypedFailure(t *testing.T) {
	runModelsFailureCases(t, []modelsFailureCase{
		{
			name: "provider unavailable",
			args: func(root, cwd string) []string {
				return []string{"--root", root, "models", "--provider", "missing:provider", "--cwd", cwd}
			},
			deps:       func(*testing.T, string) Dependencies { return Dependencies{} },
			wantExit:   2,
			wantStatus: "unavailable",
			wantReason: "discovery_unavailable",
			wantFail:   DispatchFailureResponse{Code: "provider_unavailable", Stage: "select-provider", NextAction: "stop_and_report"},
			wantError:  "The selected provider is not available in this environment.",
		},
		{
			name:       "invalid working directory",
			args:       func(root, _ string) []string { return modelsFixtureArgs(root, root) },
			deps:       modelsFixtureDeps(nil),
			wantExit:   2,
			wantStatus: "failed",
			wantReason: "discovery_failed",
			wantFail:   DispatchFailureResponse{Code: "invalid_request", Stage: "validate-request", NextAction: "correct_request"},
			wantError:  "Delegate could not accept the dispatch request.",
		},
		{
			name:       "provider preparation",
			args:       modelsFixtureArgs,
			deps:       modelsFixtureDeps(modelsFailureLeaky(errors.New("provider executable rejected"))),
			wantExit:   1,
			wantStatus: "failed",
			wantReason: "discovery_failed",
			wantFail:   DispatchFailureResponse{Code: "provider_preparation_failed", Stage: "prepare-provider", NextAction: "stop_and_report"},
			wantError:  neutralFailureMessage,
			forbidden:  []string{modelsFailurePrivatePath, "provider executable rejected"},
		},
		{
			name: "supervisor setup",
			args: modelsFixtureArgs,
			deps: func(t *testing.T, cwd string) Dependencies {
				bundle := testsupport.CanonicalTempDir(t)
				runner := filepath.Join(bundle, "delegate-run")
				writeRunnerFixture(t, runner, "runner")
				return Dependencies{Catalog: modelsFailureCatalog(t, cwd, nil), RunnerExecutable: runner, InitialSupervisorExecutable: filepath.Join(bundle, "missing-supervisor")}
			},
			wantExit:   2,
			wantStatus: "unavailable",
			wantReason: "discovery_unavailable",
			wantFail:   DispatchFailureResponse{Code: "supervisor_setup_failed", Stage: "prepare-supervisor-executables", NextAction: "stop_and_report"},
			wantError:  "Delegate could not prepare its private task supervisor.",
			forbidden:  []string{"missing-supervisor"},
		},
	})
}

// The supervisor refusals below are injected through the provider-prepare
// boundary, so their stage is prepare-provider. The real start-supervisor
// refusal is covered by TestModelsRealPrivateSupervisorMigrationRefusalIsTyped.
func TestModelsSupervisorRefusalExposesTypedFailure(t *testing.T) {
	recovery := func(reason string, cause error) error {
		return modelsFailureLeaky(&pueue.PrivateRecoveryError{ReasonCode: reason, Err: cause})
	}
	runModelsFailureCases(t, []modelsFailureCase{
		{
			name:       "active work blocks migration",
			args:       modelsFixtureArgs,
			deps:       modelsFixtureDeps(recovery("active_work", pueue.ErrPrivateSupervisorMigrationBusy)),
			wantExit:   2,
			wantStatus: "blocked",
			wantReason: "supervisor_migration_blocked",
			wantFail:   DispatchFailureResponse{Code: "supervisor_migration_blocked", ReasonCode: "active_work", Stage: "prepare-provider", NextAction: "wait_for_legacy_work"},
			wantError:  "Delegate found active work on an older private supervisor and could not migrate it safely. Wait for queued or running work before migrating.",
			forbidden:  []string{modelsFailurePrivatePath},
		},
		{
			name:       "external supervisor blocks migration",
			args:       modelsFixtureArgs,
			deps:       modelsFixtureDeps(modelsFailureLeaky(inspection.ErrGroupMigrationUnsupported)),
			wantExit:   2,
			wantStatus: "blocked",
			wantReason: "supervisor_migration_unsupported",
			wantFail:   DispatchFailureResponse{Code: "supervisor_migration_unsupported", Stage: "prepare-provider", NextAction: "stop_and_report"},
			wantError:  "Delegate found an external supervisor binding that it must not stop or migrate.",
			forbidden:  []string{modelsFailurePrivatePath},
		},
		{
			name:       "pending transition requires migration",
			args:       modelsFixtureArgs,
			deps:       modelsFixtureDeps(recovery("transition_pending", pueue.ErrPrivateSupervisorMigrationRequired)),
			wantExit:   2,
			wantStatus: "blocked",
			wantReason: "supervisor_upgrade_required",
			wantFail: DispatchFailureResponse{
				Code: "supervisor_upgrade_required", ReasonCode: "transition_pending", Stage: "prepare-provider", NextAction: "run_supervisor_migrate",
				RecoveryCommand: "delegate supervisor migrate --root <same-state-root> --json",
			},
			wantError: "Delegate found supervisor state that requires safe migration before dispatch. A saved supervisor transition must finish before admission.",
			forbidden: []string{modelsFailurePrivatePath},
		},
	})
}

func TestModelsStateRootFailureNamesItsStage(t *testing.T) {
	blocker := filepath.Join(testsupport.CanonicalTempDir(t), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := testsupport.CanonicalTempDir(t)
	assertModelsFailure(t, modelsFailureCase{
		wantExit:   1,
		wantStatus: "failed",
		wantReason: "discovery_failed",
		wantFail:   DispatchFailureResponse{Code: "state_root_unavailable", Stage: "resolve-state-root", NextAction: "stop_and_report"},
		wantError:  "Delegate could not access the saved task state. Stop and report this failure.",
		forbidden:  []string{blocker},
	}, modelsFixtureArgs(filepath.Join(blocker, "state"), cwd), Dependencies{Catalog: modelsFailureCatalog(t, cwd, nil)})
}

func TestModelsRealPrivateSupervisorMigrationRefusalIsTyped(t *testing.T) {
	store, deps, original := newSupervisorProbeTestFixture(t)
	initial, err := preparePrivateAdmissionSupervisor(deps, store, deps.SupervisorOptions)
	settleSupervisorProbeTestClient(t, initial, err)
	if err != nil || initial == nil {
		t.Fatalf("initial private admission failed: client=%v err=%v", initial, err)
	}
	active := initial.Binding()
	if closeErr := initial.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	stagePublishedGenerationSuccessor(t, store.Root, active, original.Binding().ClientExecutable, original.Binding().DaemonExecutable)
	stopActiveGenerationEndpoint(t, active)
	if err = os.Remove(active.ClientExecutable); err != nil {
		t.Fatal(err)
	}
	cwd := testsupport.CanonicalTempDir(t)
	runner := filepath.Join(testsupport.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, runner, "runner")
	deps.Catalog, deps.RunnerExecutable = modelsFailureCatalog(t, cwd, nil), runner

	assertModelsFailure(t, modelsFailureCase{
		wantExit:   2,
		wantStatus: "blocked",
		wantReason: "supervisor_upgrade_required",
		wantFail: DispatchFailureResponse{
			Code: "supervisor_upgrade_required", Stage: "start-supervisor", NextAction: "run_supervisor_migrate",
			RecoveryCommand: "delegate supervisor migrate --root <same-state-root> --json",
		},
		wantError: "Delegate found supervisor state that requires safe migration before dispatch.",
		forbidden: []string{store.Root, active.ConfigPath},
	}, modelsFixtureArgs(store.Root, cwd), deps)
}

// assertModelsFailure checks the whole public contract of one failed models
// command in JSON and human form.
func assertModelsFailure(t *testing.T, want modelsFailureCase, args []string, deps Dependencies) {
	t.Helper()
	response, jsonExit, human, humanExit := runModelsPublic(t, args, deps)
	if jsonExit != want.wantExit || humanExit != want.wantExit {
		t.Fatalf("exit json=%d human=%d, want %d: %+v", jsonExit, humanExit, want.wantExit, response)
	}
	if response.Status != want.wantStatus || response.ReasonCode != want.wantReason || response.Complete || len(response.Models) != 0 || response.Source != "none" {
		t.Fatalf("catalog changed: %+v", response.ModelCatalog)
	}
	if response.Failure == nil || *response.Failure != want.wantFail {
		t.Fatalf("failure = %+v, want %+v", response.Failure, want.wantFail)
	}
	if response.Error != want.wantError {
		t.Fatalf("error = %q, want %q", response.Error, want.wantError)
	}
	assertModelsFailureHuman(t, want, response.Provider, human)
	for _, secret := range want.forbidden {
		if strings.Contains(human, secret) || strings.Contains(response.Error, secret) {
			t.Fatalf("public output exposed %q:\n%s", secret, human)
		}
	}
}

func assertModelsFailureHuman(t *testing.T, want modelsFailureCase, provider, human string) {
	t.Helper()
	failure := want.wantFail
	for _, line := range []string{
		fmt.Sprintf("%s: %s (%s)\n", provider, want.wantStatus, want.wantReason),
		fmt.Sprintf("failure: %s (stage: %s, next action: %s)\n", failure.Code, failure.Stage, failure.NextAction),
		fmt.Sprintf("error: %s\n", want.wantError),
		"Harness effort choices: unknown (model compatibility may vary)\n",
	} {
		if !strings.Contains(human, line) {
			t.Fatalf("human output lacks %q:\n%s", line, human)
		}
	}
	if (failure.ReasonCode != "") != strings.Contains(human, "reason: "+failure.ReasonCode+"\n") {
		t.Fatalf("human reason line disagrees with %q:\n%s", failure.ReasonCode, human)
	}
	if (failure.RecoveryCommand != "") != strings.Contains(human, "recovery: "+failure.RecoveryCommand+"\n") {
		t.Fatalf("human recovery line disagrees with %q:\n%s", failure.RecoveryCommand, human)
	}
}
