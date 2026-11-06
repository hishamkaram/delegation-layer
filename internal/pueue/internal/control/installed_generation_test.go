package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInstalledCallbackPreservesLexicalSelectionAndStrictArguments(t *testing.T) {
	root := filepath.Join(canonicalTemp(t), "state")
	base := filepath.Join(root, privateSupervisorDirectory)
	binding := task.SupervisorRef{ExecutableIdentity: task.ExecutableIdentityInstalledPathV1}
	for _, runner := range []string{"/opt/candidate/bin/delegate-run", "/opt/version link/delegate-run", "/opt/quote'and!bang/delegate-run", "/opt/line\nbreak/delegate-run"} {
		callback, err := privateInstalledTerminalEventCallback(root, runner)
		if err != nil {
			t.Fatal(err)
		}
		got, valid := privateTerminalCallbackRunner(base, &callback)
		if !valid || got != runner || !privateTerminalEventsConfiguredForBinding(base, &callback, binding) {
			t.Fatalf("selected callback path changed: got=%q valid=%t callback=%q", got, valid, callback)
		}
		for _, changed := range []string{callback + "; false", strings.Replace(callback, "--terminal-event", "--another-operation", 1), callback + " extra"} {
			if privateTerminalEventsConfiguredForBinding(base, &changed, binding) {
				t.Fatalf("malformed callback accepted: %q", changed)
			}
		}
	}
	legacyCallback := privateTerminalEventCallback(base)
	if privateTerminalEventsConfiguredForBinding(base, &legacyCallback, binding) {
		t.Fatal("new generation accepted historical state helper")
	}
	if !privateTerminalEventsConfiguredForBinding(base, &legacyCallback, task.SupervisorRef{}) {
		t.Fatal("historical callback lost its exact contract")
	}
}

func TestMarkedDaemonPublicationPreservesCompleteIdentity(t *testing.T) {
	root, refs := newPrivateSuccessorTestState(t)
	base := filepath.Join(root, privateSupervisorDirectory)
	identity := privateDaemonIdentityForBinding(refs["A"])
	if err := publishDaemonOnlyIdentity(base, identity); err != nil {
		t.Fatal(err)
	}
	got, found, err := loadPrivateDaemonIdentity(base)
	if err != nil || !found || got != identity {
		t.Fatalf("daemon publication lost marker or config identity: found=%t got=%+v err=%v", found, got, err)
	}
	truncated := privateDaemonIdentity{ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, Executable: identity.Executable}
	if publicationErr := publishDaemonOnlyIdentity(base, truncated); publicationErr == nil {
		t.Fatal("marked generation published without canonical config identity")
	}
	after, _, err := loadPrivateDaemonIdentity(base)
	if err != nil || after != identity {
		t.Fatalf("invalid publication replaced canonical identity: %+v %v", after, err)
	}
}

func TestHistoricalCanonicalIdentityAcceptsInstalledRuntimeWithoutByteGate(t *testing.T) {
	_, saved := installedIdentityLegacyRef(t)
	canonical := privateDaemonIdentityForBinding(saved)
	runtime := saved
	runtime.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	runtime.ClientExecutable, runtime.DaemonExecutable = "/installed/pueue", "/installed/pueued"
	runtime.ClientSHA256, runtime.DaemonSHA256 = "", ""
	if !privateMigrationIdentityMatchesCurrent(canonical, privateDaemonIdentityForBinding(runtime), canonical) {
		t.Fatal("installed runtime invented historical generation drift")
	}
	runtime.ConfigDigest = strings.Repeat("f", 64)
	if privateMigrationIdentityMatchesCurrent(canonical, privateDaemonIdentityForBinding(runtime), canonical) {
		t.Fatal("changed runtime config relabeled as historical generation")
	}
}

func TestHistoricalGenerationLaunchRefusesBeforeFilesystemOrNativeWork(t *testing.T) {
	root, saved := installedIdentityLegacyRef(t)
	client := &Client{binding: saved, runtimeBinding: saved}
	if err := client.validatePrivateGenerationLaunch(root); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) || !errors.Is(err, ErrHistoricalTargetNotReady) || privateRecoveryReason(err) != "historical_target_not_ready" {
		t.Fatalf("historical restart lost its non-retryable refusal: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical restart created state: %v", err)
	}
	if err := client.startPrivate(context.Background(), root); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("historical restart passed through native launch: %v", err)
	}
}

func TestAcceptedMigrationTargetCannotBeReplacedByInstalledSelection(t *testing.T) {
	root, refs := newPrivateSuccessorTestState(t)
	base := filepath.Join(root, privateSupervisorDirectory)
	record := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(refs["A"]), Successor: privateDaemonIdentityForBinding(refs["B"])}
	if err := publishPrivateMigrationState(base, record); err != nil {
		t.Fatal(err)
	}
	before := readPrivateSupervisorFile(t, privateMigrationRecordPath(base))
	replacement := record
	replacement.Successor = privateDaemonIdentityForBinding(refs["C"])
	if err := replacePrivateMigrationRecord(base, record, replacement); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("accepted migration target was replaceable: %v", err)
	}
	after := readPrivateSupervisorFile(t, privateMigrationRecordPath(base))
	if string(after) != string(before) {
		t.Fatal("accepted migration target bytes changed")
	}
}

func TestInstalledCallbackConfigRetainsOtherManagedSettings(t *testing.T) {
	base := filepath.Join(canonicalTemp(t), "state", privateSupervisorDirectory)
	original := []byte(strings.Replace(privateConfigYAML(base), "daemon:\n", "daemon:\n  compress_state_file: true\n", 1))
	runner := "/opt/selected version/delegate-run"
	changed, updated, err := privateInstalledCallbackConfig(original, base, runner)
	if err != nil || !updated {
		t.Fatalf("owned callback update: updated=%t err=%v", updated, err)
	}
	old := "  callback: " + strconv.Quote(privateTerminalEventCallback(base))
	callback, err := privateInstalledTerminalEventCallback(filepath.Dir(base), runner)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(original), old, "  callback: "+strconv.Quote(callback), 1)
	if string(changed) != want {
		t.Fatal("callback conversion changed unrelated managed config bytes")
	}
	again, updated, err := privateInstalledCallbackConfig(changed, base, runner)
	if err != nil || updated || string(again) != string(changed) {
		t.Fatalf("callback update was not idempotent: changed=%t err=%v", updated, err)
	}
}
