package supervisor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func infrastructureSourceFixture(t *testing.T) (string, task.SupervisorRef) {
	t.Helper()
	root := supervisorfixture.CanonicalTempDir(t)
	client, daemon := statePairStagingSources(t, "published-client", "published-daemon")
	client, daemon, err := InstallStateSupervisorPairPaths(t.Context(), root, client, daemon)
	if err != nil {
		t.Fatal(err)
	}
	binding := generationBinding(root, "infrastructure")
	pair := savedSupervisorPairRef(t, client, daemon)
	binding.ClientExecutable, binding.ClientSHA256 = pair.ClientExecutable, pair.ClientSHA256
	binding.DaemonExecutable, binding.DaemonSHA256 = pair.DaemonExecutable, pair.DaemonSHA256
	if err := pueue.WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		return pueue.PublishPrivateSupervisorBindingIfAbsentContext(ctx, root, binding)
	}); err != nil {
		t.Fatal(err)
	}
	return root, binding
}

func TestInfrastructurePairSourceUsesPublishedBinding(t *testing.T) {
	root, binding := infrastructureSourceFixture(t)
	nextClient, nextDaemon := statePairStagingSources(t, "inventory-client", "inventory-daemon")
	if err := installStateSupervisorPair(t.Context(), root, nextClient, nextDaemon); err != nil {
		t.Fatal(err)
	}
	source, err := readInfrastructurePairSource(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if source.saved != binding {
		t.Fatalf("source did not select published binding: %+v", source.saved)
	}
	// No daemon exists at the binding's endpoint. Neither read nor stage needs it.
	target := supervisorfixture.CanonicalTempDir(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// Exclusive source ownership proves read released its shared lease; staging
	// inside this owner also proves it never reacquires that source lease.
	if err := pueue.WithBootstrapLockContext(ctx, root, func(locked context.Context) error {
		client, daemon, err := source.stage(locked, target)
		if err != nil {
			return err
		}
		supervisorfixture.RequireExecutable(t, client, "published-client")
		supervisorfixture.RequireExecutable(t, daemon, "published-daemon")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requireStatePairSelectionUnchanged(t, filepath.Join(target, ".supervisor"), nil, false)
	if _, found, err := pueue.ReadPrivateSupervisorBinding(target); err != nil || found {
		t.Fatalf("stage published target binding: found=%v err=%v", found, err)
	}
}

func TestInfrastructurePairSourcePreservesTargetSelection(t *testing.T) {
	root, _ := infrastructureSourceFixture(t)
	target, binding := infrastructureSourceFixture(t)
	base := filepath.Join(target, ".supervisor")
	bindingPath := filepath.Join(base, "supervisor.identity.json")
	beforeBinding, err := os.ReadFile(bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := readInfrastructurePairSource(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately choose a different target inventory while preserving binding.
	client, daemon := statePairStagingSources(t, "target-client", "target-daemon")
	if err = installStateSupervisorPair(t.Context(), target, client, daemon); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(base, stateSupervisorPairRecordName))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = source.stage(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	requireStatePairSelectionUnchanged(t, base, before, true)
	after, err := os.ReadFile(bindingPath)
	if err != nil || !bytes.Equal(beforeBinding, after) {
		t.Fatalf("target binding changed: %v", err)
	}
	current, found, err := pueue.ReadPrivateSupervisorBinding(target)
	if err != nil || !found || current != binding {
		t.Fatalf("target binding identity changed: %v", err)
	}
}

func TestInfrastructurePairSourceRejectsInvalidBinding(t *testing.T) {
	for _, kind := range []string{"missing", "unreadable", "nonstate", "mixed", "tampered"} {
		t.Run(kind, func(t *testing.T) {
			root, binding := infrastructureSourceFixture(t)
			changeInfrastructureBindingFixture(t, root, binding, kind)
			_, err := readInfrastructurePairSource(t.Context(), root)
			want := pueue.ErrBinding
			if kind == "tampered" {
				want = pueue.ErrConfiguration
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s: expected %v, got %v", kind, want, err)
			}
			if kind == "missing" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing cause lost: %v", err)
			}
		})
	}
}

func TestInfrastructurePairSourceRejectsZeroAndChangedSource(t *testing.T) {
	target := supervisorfixture.CanonicalTempDir(t)
	if _, _, err := (infrastructurePairSource{}).stage(t.Context(), target); !errors.Is(err, pueue.ErrBinding) {
		t.Fatalf("zero source: %v", err)
	}
	root, binding := infrastructureSourceFixture(t)
	source, err := readInfrastructurePairSource(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(supervisorfixture.CanonicalTempDir(t), "replacement")
	supervisorfixture.WriteExecutable(t, replacement, "replacement bytes")
	if err = os.Rename(replacement, binding.ClientExecutable); err != nil {
		t.Fatal(err)
	}
	if _, _, err = source.stage(t.Context(), target); !errors.Is(err, pueue.ErrConfiguration) {
		t.Fatalf("changed source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid source mutated target: %v", err)
	}
}

func changeInfrastructureBindingFixture(t *testing.T, root string, binding task.SupervisorRef, kind string) {
	t.Helper()
	path := filepath.Join(root, ".supervisor", "supervisor.identity.json")
	switch kind {
	case "missing":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	case "unreadable":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	case "nonstate", "mixed":
		client, daemon := statePairStagingSources(t, "external-client", "external-daemon")
		external := savedSupervisorPairRef(t, client, daemon)
		binding.ClientExecutable, binding.ClientSHA256 = external.ClientExecutable, external.ClientSHA256
		if kind == "nonstate" {
			binding.DaemonExecutable, binding.DaemonSHA256 = external.DaemonExecutable, external.DaemonSHA256
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := pueue.PublishPrivateSupervisorBindingIfAbsentContext(t.Context(), root, binding); err != nil {
			t.Fatal(err)
		}
	case "tampered":
		if err := os.WriteFile(binding.ClientExecutable, []byte("changed"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}
