package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type generationGuardFunc func(context.Context, string, func() error) error

func (guard generationGuardFunc) GuardPrivateGenerationCommit(ctx context.Context, root string, commit func() error) error {
	return guard(ctx, root, commit)
}

type generationGuardFixture struct {
	root   string
	base   string
	client *Client
	record privateMigrationRecord
}

// This fixture binds executable/config identities only. It starts no daemon;
// every short fixture control command is joined by the client on cleanup.
func newGenerationGuardFixture(t *testing.T) generationGuardFixture {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	options := Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}}
	resolution, err := privateResolution(options)
	if err != nil {
		t.Fatal(err)
	}
	config, base, err := ensurePrivateConfigContext(t.Context(), fixture.stateRoot, resolution, options.PrivateGenerationGuard, "")
	if err != nil {
		t.Fatal(err)
	}
	client, err := Bind(t.Context(), fixture.clientPath, config, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := attachPrivateDaemon(client, fixture.daemonPath); err != nil {
		t.Fatal(err)
	}
	legacy := privateDaemonIdentityForBinding(client.Binding())
	successor := legacy
	successor.Executable = filepath.Join(filepath.Dir(fixture.daemonPath), "successor-pueued")
	return generationGuardFixture{
		root: fixture.stateRoot, base: base, client: client,
		record: privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: legacy, Successor: successor},
	}
}

func TestPrivateGenerationGuardDeniesPublicationTransactions(t *testing.T) {
	for _, name := range []string{"identity", "binding", "binding-replacement", "migration-create", "migration-replacement", "migration-completion", "recovery-alias"} {
		t.Run(name, func(t *testing.T) {
			fixture := newGenerationGuardFixture(t)
			operation := generationGuardOperation(t, fixture, name)
			before := snapshotPrivateSuccessorTestState(t, fixture.root)
			denied := errors.New("resident generation revoked")
			calls := 0
			guard := generationGuardFunc(func(ctx context.Context, root string, _ func() error) error {
				calls++
				if ctx != t.Context() || root != fixture.root {
					t.Errorf("guard received a different operation context/root")
				}
				return denied
			})
			if err := operation(guard); !errors.Is(err, denied) || calls != 1 {
				t.Fatalf("publication did not honor denied resident grant: calls=%d err=%v", calls, err)
			}
			if after := snapshotPrivateSuccessorTestState(t, fixture.root); !reflect.DeepEqual(before, after) {
				t.Fatal("denied publication changed durable generation state")
			}
			if err := operation(nil); !errors.Is(err, ErrInfrastructureOwnerRequired) {
				t.Fatalf("nil resident grant authorized a new publication: %v", err)
			}
			if after := snapshotPrivateSuccessorTestState(t, fixture.root); !reflect.DeepEqual(before, after) {
				t.Fatal("nil-grant publication changed durable generation state")
			}
		})
	}
}

func generationGuardOperation(t *testing.T, fixture generationGuardFixture, name string) func(PrivateGenerationGuard) error {
	t.Helper()
	binding := fixture.client.Binding()
	switch name {
	case "identity":
		return func(guard PrivateGenerationGuard) error {
			return publishPrivateDaemonIdentityContext(t.Context(), fixture.base, binding, guard)
		}
	case "binding":
		return func(guard PrivateGenerationGuard) error {
			return publishPrivateSupervisorBindingIfAbsentContext(t.Context(), fixture.root, binding, guard)
		}
	case "binding-replacement":
		return func(guard PrivateGenerationGuard) error {
			return replaceReconciledPrivateBindingContext(t.Context(), fixture.root, binding, binding, guard)
		}
	case "migration-create":
		return func(guard PrivateGenerationGuard) error {
			return publishPrivateMigrationStateContext(t.Context(), fixture.base, fixture.record, guard)
		}
	case "migration-replacement":
		if err := publishPrivateMigrationState(fixture.base, fixture.record); err != nil {
			t.Fatal(err)
		}
		replacement := fixture.record
		replacement.Successor.Executable += "-replacement"
		return func(guard PrivateGenerationGuard) error {
			return replacePrivateMigrationRecordContext(t.Context(), fixture.base, fixture.record, replacement, guard)
		}
	case "migration-completion":
		return func(guard PrivateGenerationGuard) error {
			fixture.client.options.PrivateGenerationGuard = guard
			return fixture.client.commitPrivateMigrationCompletion(t.Context(), fixture.root, fixture.base, completedGenerationGuardRecord(fixture))
		}
	case "recovery-alias":
		return func(guard PrivateGenerationGuard) error {
			fixture.client.options.PrivateGenerationGuard = guard
			return fixture.client.PersistPrivateRecoveryBindingContext(t.Context(), fixture.root)
		}
	default:
		t.Fatalf("unknown publication case %q", name)
		return nil
	}
}

func TestPrivateGenerationGuardConfigAndPrepareRefuseColdPublication(t *testing.T) {
	for _, prepare := range []bool{false, true} {
		fixture := newPrivateSupervisorTestFixture(t)
		options := Options{}
		resolution, err := privateResolution(options)
		if err != nil {
			t.Fatal(err)
		}
		if prepare {
			_, err = PreparePrivate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
		} else {
			_, _, err = ensurePrivateConfigContext(t.Context(), fixture.stateRoot, resolution, nil)
		}
		if !errors.Is(err, ErrInfrastructureOwnerRequired) {
			t.Fatalf("cold config bypassed resident grant: %v", err)
		}
		for _, path := range []string{PrivateConfigPath(fixture.stateRoot), privateBootstrapRecordPath(fixture.stateRoot)} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unowned cold config published %s: %v", filepath.Base(path), err)
			}
		}
	}
}

func TestPrivateGenerationGuardRepairAuthorizationRunsInsideCommit(t *testing.T) {
	root, resolution := newLongDarwinPrivateStateRoot(t)
	base := filepath.Join(root, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		t.Fatal(err)
	}
	if err := markPrivateBootstrapPending(root); err != nil {
		t.Fatal(err)
	}
	path := PrivateConfigPath(root)
	original := privateConfigYAML(base)
	if err := createPrivateConfig(path, base, original); err != nil {
		t.Fatal(err)
	}
	calls := 0
	guard := generationGuardFunc(func(_ context.Context, _ string, commit func() error) error {
		calls++
		// A generation appearing at the authority boundary revokes repair.
		if err := publishDaemonOnlyIdentity(base, privateDaemonIdentity{Executable: filepath.Join(base, "pueued"), SHA256: strings.Repeat("a", 64)}); err != nil {
			return err
		}
		return commit()
	})
	if _, _, err := ensurePrivateConfigContext(t.Context(), root, resolution, guard); !errors.Is(err, errUnixSocketPathTooLong) || calls != 1 {
		t.Fatalf("repair ignored in-commit generation evidence: calls=%d err=%v", calls, err)
	}
	if got := string(readPrivateSupervisorFile(t, path)); got != original {
		t.Fatal("repair changed config after a generation appeared inside the grant")
	}
}

func TestPrivateGenerationGuardStartMarkerPrecedesNoOrchestration(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	if err := clearPrivateBootstrapPending(fixture.root); err != nil {
		t.Fatal(err)
	}
	fixture.client.options.PrivateGenerationGuard = nil
	if err := fixture.client.startPrivate(t.Context(), fixture.root); !errors.Is(err, ErrInfrastructureOwnerRequired) {
		t.Fatalf("start marker bypassed generation grant: %v", err)
	}
	if pending, err := PrivateBootstrapPending(fixture.root); err != nil || pending {
		t.Fatalf("refused start left a bootstrap marker: pending=%t err=%v", pending, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.base, "daemon-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused marker reached daemon start: %v", err)
	}
}

func TestPrivatePreparedTargetMismatchRefusesBeforeGenerationGrant(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	binding := fixture.client.Binding()
	if err := commitPrivateDaemonIdentity(fixture.base, binding); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateMigrationState(fixture.base, fixture.record); err != nil {
		t.Fatal(err)
	}
	before := snapshotPrivateSuccessorTestState(t, fixture.root)
	fixture.client.options.PrivateGenerationGuard = nil
	identity := privateDaemonIdentityForBinding(binding)
	err := fixture.client.resumeValidatedPreparedPrivateMigration(t.Context(), fixture.root, fixture.base, binding, true, identity, true, identity, identity, fixture.record)
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("fixed target mismatch did not refuse: %v", err)
	}
	if after := snapshotPrivateSuccessorTestState(t, fixture.root); !reflect.DeepEqual(before, after) {
		t.Fatal("fixed target mismatch erased prepared migration")
	}
}

func TestPrivateGenerationGuardExactReplayAndWarmReadNeedNoGrant(t *testing.T) {
	fixture := newGenerationGuardFixture(t)
	binding := fixture.client.Binding()
	data, marshalErr := task.MarshalCanonical(privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: binding})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err := publishPrivateRecord(fixture.base, privateIdentityPublicationPath(fixture.root), data); err != nil {
		t.Fatal(err)
	}
	if err := WithBootstrapLockContext(t.Context(), fixture.root, func(context.Context) error {
		replayed, err := recoverPrivateIdentityPublication(fixture.root)
		if err != nil || !replayed {
			t.Fatalf("exact pending body was not replayed: replayed=%t err=%v", replayed, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateSupervisorBindingIfAbsentContext(t.Context(), fixture.root, binding, nil); err != nil {
		t.Fatalf("equivalent durable binding required a grant: %v", err)
	}
	resolution, err := privateResolution(fixture.client.options)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensurePrivateConfigContext(t.Context(), fixture.root, resolution, nil); err != nil {
		t.Fatalf("valid existing config required a grant: %v", err)
	}
	if err := publishPrivateMigrationState(fixture.base, fixture.record); err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateMigrationStateContext(t.Context(), fixture.base, fixture.record, nil); err != nil {
		t.Fatalf("exact durable migration required a grant: %v", err)
	}
}

func completedGenerationGuardRecord(fixture generationGuardFixture) privateMigrationRecord {
	record := fixture.record
	record.Legacy, record.Successor = record.Successor, record.Legacy
	record.LegacyPID = 123
	record.LegacyProcessStart = "fixture-prior-process"
	record.LegacyShutdownRequested = true
	record.LegacyShutdownDispatched = true
	record.LegacyStopped = true
	return record
}
