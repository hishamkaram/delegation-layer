package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInheritedControlLeasesCloseOnExec(t *testing.T) {
	original, err := os.OpenFile(filepath.Join(t.TempDir(), "lease"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := original.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	raw, err := unix.FcntlInt(original.Fd(), unix.F_DUPFD, 128)
	if err != nil {
		t.Fatal(err)
	}
	var inherited []*os.File
	t.Cleanup(func() {
		if len(inherited) > 0 {
			if closeErr := closeControlLeaseFiles(inherited); closeErr != nil {
				t.Error(closeErr)
			}
		} else {
			if closeErr := unix.Close(raw); closeErr != nil && !errors.Is(closeErr, unix.EBADF) {
				t.Error(closeErr)
			}
		}
	})
	before, err := unix.FcntlInt(uintptr(raw), unix.F_GETFD, 0)
	if err != nil || before&unix.FD_CLOEXEC != 0 {
		t.Fatalf("fixture must start with non-CLOEXEC descriptor: flags=%d err=%v", before, err)
	}
	inherited, err = inheritedControlLeaseFiles(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(inherited[0].Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("inherited lease remains non-CLOEXEC before reconstructed child Start")
	}
}

func TestInheritedControlLeaseFailureClosesEarlierDescriptors(t *testing.T) {
	original, err := os.OpenFile(filepath.Join(t.TempDir(), "lease"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := original.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	first, err := unix.FcntlInt(original.Fd(), unix.F_DUPFD, 128)
	if err != nil {
		t.Fatal(err)
	}
	second, err := unix.FcntlInt(original.Fd(), unix.F_DUPFD, first+1)
	if err != nil {
		t.Fatal(err)
	}
	if second != first+1 {
		t.Fatal("fixture descriptors are not contiguous")
	}
	if err = unix.Close(second); err != nil {
		t.Fatal(err)
	}
	files, err := inheritedControlLeaseFiles(first, 2)
	if err == nil {
		t.Fatal("closed inherited descriptor was accepted")
	}
	if len(files) != 0 {
		t.Fatal("failed inheritance returned unreachable partial ownership")
	}
	if _, inspectErr := unix.FcntlInt(uintptr(first), unix.F_GETFD, 0); !errors.Is(inspectErr, unix.EBADF) {
		t.Fatalf("earlier wrapped descriptor was not closed: %v", inspectErr)
	}
}

func newShutdownConstructionFixture(t *testing.T) (privateSupervisorTestFixture, *Client) {
	t.Helper()
	return newRevocableShutdownFixture(t)
}

func TestPrivateShutdownRequiresCanonicalConstructionAndActualEX(t *testing.T) {
	fixture, canonical := newShutdownConstructionFixture(t)
	if _, err := canonical.privateShutdownContext(t.Context()); !errors.Is(err, ErrBinding) {
		t.Fatalf("construction alone granted shutdown forwarding: %v", err)
	}
	if err := withPublishedPrivateLockContext(t.Context(), fixture.stateRoot, "bootstrap.lock", unix.LOCK_SH, func(shared context.Context) error {
		if _, scopeErr := canonical.privateShutdownContext(shared); !errors.Is(scopeErr, ErrBinding) {
			t.Fatalf("SH granted EX classification: %v", scopeErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plain, err := NewClient(canonical.Binding(), canonical.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := plain.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lookalike := newShutdownLookalike(t, fixture, canonical)
	if err = WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(locked context.Context) error {
		if _, scopeErr := canonical.privateShutdownContext(locked); scopeErr != nil {
			return scopeErr
		}
		for name, client := range map[string]*Client{"saved identity only": plain, "staged-path lookalike": lookalike} {
			if _, scopeErr := client.privateShutdownContext(locked); !errors.Is(scopeErr, ErrBinding) {
				t.Fatalf("%s minted provenance: %v", name, scopeErr)
			}
		}
		changed := *canonical
		changed.runtimeBinding.Endpoint += "changed"
		if _, scopeErr := changed.privateShutdownContext(locked); !errors.Is(scopeErr, ErrBinding) {
			t.Fatalf("changed runtime retained construction authority: %v", scopeErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func newShutdownLookalike(t *testing.T, fixture privateSupervisorTestFixture, canonical *Client) *Client {
	t.Helper()
	lookalikePath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, ".migration-pueue-lookalike.yml")
	data, err := os.ReadFile(canonical.Binding().ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(lookalikePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	lookalike, err := Bind(t.Context(), fixture.clientPath, lookalikePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lookalike.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lookalike.options.ControlOwnerExecutable = os.Args[0]
	return lookalike
}

func TestPrivateShutdownSelectsTaggedBootstrapAmongMergedLeases(t *testing.T) {
	fixture, client := newShutdownConstructionFixture(t)
	decoyPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "decoy.lock")
	if err := os.WriteFile(decoyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := withPublishedPrivateLockContext(t.Context(), fixture.stateRoot, "decoy.lock", unix.LOCK_EX, func(outer context.Context) error {
		return WithBootstrapLockContext(outer, fixture.stateRoot, func(locked context.Context) (resultErr error) {
			scoped, err := client.privateShutdownContext(locked)
			if err != nil {
				return err
			}
			duplicates, err := duplicateControlLeaseFiles(scoped)
			files := duplicates
			if err != nil {
				return err
			}
			defer func() { resultErr = errors.Join(resultErr, closeControlLeaseFiles(files)) }()
			record, err := privateShutdownForwarding(scoped, files)
			if err != nil {
				return err
			}
			if record == nil || record.LeaseIndex != 1 {
				t.Fatalf("selected first/merged lease instead of tagged EX: %+v", record)
			}
			assertShutdownSelectedDescriptor(t, record, files)
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertShutdownSelectedDescriptor(t *testing.T, record *controlOwnerShutdown, files []*os.File) {
	t.Helper()
	selected, err := controlOwnerShutdownFiles(controlOwnerRequest{Shutdown: record}, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0] != files[1] {
		t.Fatal("forwarding included a merged lease")
	}
	wrong := *record
	wrong.LeaseIndex = 0
	if _, selectErr := controlOwnerShutdownFiles(controlOwnerRequest{Shutdown: &wrong}, files); selectErr == nil {
		t.Fatal("decoy descriptor accepted as tagged bootstrap")
	}
	wrong = *record
	wrong.Bootstrap.Inode++
	if _, selectErr := controlOwnerShutdownFiles(controlOwnerRequest{Shutdown: &wrong}, files); !errors.Is(selectErr, ErrBinding) {
		t.Fatalf("changed descriptor identity accepted: %v", selectErr)
	}
}

func TestControlOwnerShutdownRejectsRetargetedCommand(t *testing.T) {
	root := filepath.Join(canonicalTemp(t), "root")
	config := filepath.Join(root, privateSupervisorDirectory, "pueue.yml")
	for _, test := range []struct {
		name      string
		arguments []string
		status    string
		index     int
	}{
		{name: "status", arguments: []string{"pueue", "-c", config, "status"}},
		{name: "extra argv", arguments: []string{"pueue", "-c", config, "shutdown", "extra"}},
		{name: "different config", arguments: []string{"pueue", "-c", config + "other", "shutdown"}},
		{name: "status owner", arguments: []string{"pueue", "-c", config, "shutdown"}, status: "4.0.4"},
		{name: "invalid descriptor", arguments: []string{"pueue", "-c", config, "shutdown"}, index: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := &controlOwnerShutdown{Root: root, ConfigPath: config, Bootstrap: controlLeaseIdentity{Device: 1, Inode: 2}, LeaseIndex: test.index}
			if err := validateControlOwnerShutdown(controlOwnerRequest{Arguments: test.arguments, StatusVersion: test.status, Shutdown: record}); err == nil {
				t.Fatal("retargeted command classified as exact private shutdown")
			}
		})
	}
}

func TestPrivateShutdownRejectsDifferentRootBootstrap(t *testing.T) {
	_, client := newShutdownConstructionFixture(t)
	other := newPrivateSupervisorTestFixture(t)
	if err := WithBootstrapLockContext(t.Context(), other.stateRoot, func(locked context.Context) error {
		if _, scopeErr := client.privateShutdownContext(locked); !errors.Is(scopeErr, ErrBinding) {
			t.Fatalf("other root bootstrap authorized shutdown: %v", scopeErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func nonIsolatedShutdownFixture(t *testing.T) (privateSupervisorTestFixture, string) {
	t.Helper()
	fixture := newPrivateSupervisorTestFixture(t)
	resolution, err := CurrentResolutionContext()
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := ensurePrivateConfigContext(t.Context(), fixture.stateRoot, resolution, allowPrivateGenerationForTest{}, privateInstalledCallbackForTest(t, fixture.stateRoot))
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(original), "show_confirmation_questions: false", "show_confirmation_questions: true", 1)
	if changed == string(original) {
		t.Fatal("fixture did not enable confirmation")
	}
	if err = os.WriteFile(config, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture, config
}

func TestBindUnqualifiedPrivateConfigPreservesReadOnlyObservation(t *testing.T) {
	fixture, config := nonIsolatedShutdownFixture(t)
	client, bindErr := Bind(t.Context(), fixture.clientPath, config, Options{})
	t.Cleanup(func() {
		if closeErr := client.CloseAfterPending(bindErr); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr != nil {
		t.Fatalf("parseable canonical config lost Bind compatibility: %v", bindErr)
	}
	if client.privateConstruction != nil {
		t.Fatal("nonisolated config acquired shutdown provenance")
	}
	if err := os.WriteFile(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Snapshot(t.Context()); err != nil {
		t.Fatalf("bound status observation changed: %v", err)
	}
	clone, err := client.WithObservationTimeout(DefaultObservationTimeout)
	if err != nil {
		t.Fatal(err)
	}
	clone.options.ControlOwnerExecutable = os.Args[0]
	if err = WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(locked context.Context) error {
		assertUnqualifiedShutdownRefused(t, locked, clone)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertUnqualifiedShutdownRefused(t *testing.T, ctx context.Context, client *Client) {
	t.Helper()
	entries := make(chan CommandEvent, 4)
	client.options.Observer = func(event CommandEvent) { entries <- event }
	dispatched, err := client.ShutdownDispatched(ctx)
	if dispatched || !errors.Is(err, ErrBinding) || !errors.Is(err, ErrConfiguration) || client.privateConstructionErr == nil || !errors.Is(err, client.privateConstructionErr) {
		t.Fatalf("unqualified shutdown dispatched/lost qualification cause: dispatched=%t err=%v", dispatched, err)
	}
	select {
	case event := <-entries:
		t.Fatalf("unqualified shutdown entered command executor: %+v", event)
	default:
	}
}

func TestRequiredPrivateShutdownConstructionRejectsNonIsolatedConfig(t *testing.T) {
	fixture, config := nonIsolatedShutdownFixture(t)
	client, bindErr := Bind(t.Context(), fixture.clientPath, config, Options{})
	t.Cleanup(func() {
		if closeErr := client.CloseAfterPending(bindErr); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr != nil {
		t.Fatal(bindErr)
	}
	if err := recordPrivateClientConstruction(client, fixture.stateRoot); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("required qualification ignored prior refusal: %v", err)
	}
	migration, err := newPrivateMigrationShutdownClient(fixture.stateRoot, client.Binding(), privateDaemonIdentityForBinding(client.Binding()), client.options)
	if migration != nil || !errors.Is(err, ErrConfiguration) {
		t.Fatalf("required migration construction accepted nonisolated config: client=%v err=%v", migration != nil, err)
	}
}
