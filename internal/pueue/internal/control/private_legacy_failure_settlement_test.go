package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func newPrivateLegacyFailureFixture(t *testing.T) (privateRecoverySettlementFixture, *Client) {
	t.Helper()
	fixture := newPrivateRecoverySettlementFixture(t)
	options := fixture.options
	options.Environment = slices.DeleteFunc(slices.Clone(options.Environment), func(value string) bool {
		return strings.HasPrefix(value, "BINDING_PROBE_GATE=")
	})
	bound, err := Bind(t.Context(), fixture.clientExecutable, fixture.record.Supervisor.ConfigPath, options)
	if err != nil {
		t.Fatal(errors.Join(err, bound.CloseAfterPending(err)))
	}
	binding := bound.Binding()
	if closeErr := bound.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	current, err := NewClient(binding, fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := current.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return fixture, current
}

func TestPrivateLegacyResidentBindFailureJoinsProbeAndCleanup(t *testing.T) {
	fixture, current := newPrivateLegacyFailureFixture(t)
	var pending *Pending
	var configAtCompletionErr error
	assertPrivateResidentBindFailureSettlement(t, fixture, func(ctx context.Context, scoped privateRecoverySettlementFixture) (*Client, error) {
		observer := scoped.options.Observer
		scoped.options.Observer = func(event CommandEvent) {
			if event.Stage == "completed" && slices.Contains(event.Argv, "--version") {
				path, pathErr := privateLegacyMigrationConfigPath(event.Argv)
				configAtCompletionErr = pathErr
				if pathErr == nil {
					_, configAtCompletionErr = os.Lstat(path)
				}
			}
			observer(event)
		}
		current.options = scoped.options
		migration, err := bindSavedEndpointMigration(ctx, scoped.record.Supervisor, current)
		pending = pendingFrom(err)
		return migration, err
	})
	if configAtCompletionErr != nil {
		t.Fatalf("temporary config was unavailable before version completion: %v", configAtCompletionErr)
	}
	path := requirePrivateLegacyMigrationConfigPath(t, pending)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resident binding returned before temporary config cleanup: %v", err)
	}
}

func TestPrivateLegacyDefaultBindFailureRetainsTemporaryConfig(t *testing.T) {
	fixture, current := newPrivateLegacyFailureFixture(t)
	migration, probeErr := detachBindingProbePendingTest(t, fixture.gate, func(ctx context.Context) (*Client, error) {
		return bindSavedEndpointMigration(ctx, fixture.record.Supervisor, current)
	})
	requirePrivateBindFailure(t, migration, probeErr)
	pending := requireBindingProbePendingTestHandle(t, probeErr, "--version")
	path := requirePrivateLegacyMigrationConfigPath(t, pending)
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("finite observer removed the config while its version probe was held: %v", err)
	}
	result := completeBindingProbePendingTest(t, fixture.gate, pending)
	if string(result.Stdout) != "pueue 99.7.3\n" {
		t.Fatalf("version probe completion=%q", result.Stdout)
	}
	awaitPrivateLegacyMigrationConfigCleanup(t, path)
}

func TestPrivateLegacySuccessfulBindOwnsCleanupOnce(t *testing.T) {
	fixture, current := newPrivateLegacyFailureFixture(t)
	fixture.gate.release()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	migration, err := bindSavedEndpointMigration(ctx, fixture.record.Supervisor, current)
	t.Cleanup(func() {
		if closeErr := migration.CloseAfterPending(err); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil || migration == nil {
		t.Fatalf("legacy migration binding: client=%v err=%v", migration, err)
	}
	path := migration.RuntimeBinding().ConfigPath
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("successful binding removed its temporary config before Close: %v", statErr)
	}
	if closeErr := migration.Close(); closeErr != nil {
		t.Fatalf("first migration close: %v", closeErr)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("migration Close did not remove its temporary config: %v", statErr)
	}
	if closeErr := migration.Close(); closeErr != nil {
		t.Fatalf("repeated migration close repeated cleanup: %v", closeErr)
	}
}

func TestPrivateLegacyNilBindFailureCleansTemporaryConfig(t *testing.T) {
	fixture, current := newPrivateLegacyFailureFixture(t)
	current.runtimeBinding.ClientExecutable = filepath.Join(t.TempDir(), "missing-pueue")
	migration, err := bindSavedEndpointMigration(t.Context(), fixture.record.Supervisor, current)
	if migration != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing migration executable: client=%v err=%v", migration, err)
	}
	paths, globErr := filepath.Glob(filepath.Join(filepath.Dir(fixture.record.Supervisor.ConfigPath), ".migration-pueue-*"))
	if globErr != nil || len(paths) != 0 {
		t.Fatalf("nil-client Bind failure retained temporary configs: %v err=%v", paths, globErr)
	}
}

func requirePrivateLegacyMigrationConfigPath(t *testing.T, pending *Pending) string {
	t.Helper()
	if pending == nil {
		t.Fatal("legacy binding lost its pending version probe")
	}
	path, err := privateLegacyMigrationConfigPath(pending.Args())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func privateLegacyMigrationConfigPath(args []string) (string, error) {
	index := slices.Index(args, "-c")
	if index < 0 || index+1 >= len(args) {
		return "", errors.New("version probe did not retain its config argument")
	}
	path := args[index+1]
	if !filepath.IsAbs(path) || !strings.HasPrefix(filepath.Base(path), ".migration-pueue-") {
		return "", errors.New("version probe did not retain an absolute temporary migration config")
	}
	return path, nil
}

func awaitPrivateLegacyMigrationConfigCleanup(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// The FIFO gates prove command ordering; this ticker only observes the
	// asynchronous finite observer's cleanup postcondition.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatalf("inspect temporary migration config: %v", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("temporary migration config was not cleaned after version completion: %v", ctx.Err())
		}
	}
}
