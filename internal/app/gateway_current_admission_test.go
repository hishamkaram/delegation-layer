package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/gatewaycommand"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Native capability/status commands and App maintenance are real. Only the SDK
// profile and service/GUI edges use the existing composition doubles. An eager
// ordinary Ensure would initialize this root and fail the independent final
// public not_initialized observation and unchanged private-state assertions.
func TestGatewayUpgradePreservesUninitializedNativeRoot(t *testing.T) {
	client, daemon := requireRuntimeRestartCLINativeContext(t)
	installation := newRuntimeRestartCLINative(t, client, daemon)
	installation.build(t)
	for _, kind := range []string{"absent", "malformed_journal", "wrong_type", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			testGatewayCurrentAdmissionRoot(t, installation, client, daemon, kind)
		})
	}
}

func testGatewayCurrentAdmissionRoot(t *testing.T, installation *runtimeRestartCLINative, client, daemon, kind string) {
	t.Helper()
	f := newGatewayCurrentAdmissionRoot(t, installation, client, daemon)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	profile := newGatewayUpgradeNativeProfile(t, ctx, f)
	original := profile.source
	seedGatewayCurrentPrivateState(t, f, kind)
	before := gatewayCurrentPrivateState(t, f.root)
	if kind == "absent" && len(before) != 0 {
		t.Fatal("fixture already contains private supervisor state")
	}
	rootBytes := readGatewayUpgradeNativeRoot(t, f)
	services := &gatewayUpgradeNativeServices{profile: profile.directory}
	if kind == "absent" {
		assertGatewayUninitializedUpgrade(t, ctx, f, profile, services)
	} else {
		requireGatewayCurrentPrivateFault(t, ctx, f, profile.source.DelegateRootContext, kind)
		runGatewayCurrentAdmissionRefusal(t, ctx, f, profile, services)
		if profile.migrated || profile.updated || profile.source.InstallationSelection() != original.InstallationSelection() || !slices.Equal(profile.source.WorkspaceIDs, original.WorkspaceIDs) {
			t.Fatal("invalid private state reached SDK migration or installation commit")
		}
		if len(services.calls) != 1 || services.calls[0] != (gatewayUpgradeNativeServiceCall{original.DelegateExecutable, "uninstall"}) {
			t.Fatal("invalid private state resumed the connector service")
		}
	}
	assertGatewayCurrentInfrastructureAbsent(t, ctx, f)
	if !bytes.Equal(rootBytes, readGatewayUpgradeNativeRoot(t, f)) || !maps.Equal(before, gatewayCurrentPrivateState(t, f.root)) {
		t.Fatal("Gateway upgrade initialized or changed private Native state or the original root record")
	}
}

func assertGatewayUninitializedUpgrade(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, profile *gatewayUpgradeNativeProfile, services *gatewayUpgradeNativeServices) {
	t.Helper()
	original := profile.source
	credential, err := profile.Status(ctx, profile.directory)
	if err != nil {
		t.Fatal(err)
	}
	response := runGatewayUpgradeNativeComposition(t, ctx, f, profile, services, "", true)
	u := response.Upgrade
	if u.Phase != "complete" || !u.ServiceRestarted || !u.DeviceIdentityPreserved || u.Supervisor != (gatewaycommand.NativeTransition{}) || !u.Native.RootContextMatches || !u.Native.CapabilitiesVerified {
		t.Fatalf("uninitialized installation did not complete without an operation: %+v", u)
	}
	assertGatewayUninitializedProfile(t, profile, original)
	if response.Credential == nil || *response.Credential != credential {
		t.Fatal("uninitialized upgrade changed the saved device identity")
	}
	services.assertHandoff(t, original.DelegateExecutable, profile.selected)
	assertGatewayUninitializedPublicStatus(t, ctx, f)
}

func assertGatewayUninitializedProfile(t *testing.T, profile *gatewayUpgradeNativeProfile, original deviceclient.LocalContextInfo) {
	t.Helper()
	if !profile.migrated || !profile.updated || profile.source.DelegateRoot != original.DelegateRoot || profile.source.DelegateRootContext != original.DelegateRootContext || profile.source.DelegateExecutable != profile.selected || !slices.Equal(profile.source.WorkspaceIDs, original.WorkspaceIDs) {
		t.Fatal("uninitialized upgrade changed the original root/profile selection")
	}
}

func assertGatewayUninitializedPublicStatus(t *testing.T, ctx context.Context, f *runtimeRestartCLINative) {
	t.Helper()
	status, code, err := f.command(ctx, "status")
	if err != nil || code != 0 || status.RootID != f.rootID || status.SupervisorControl == nil || status.SupervisorControl.State != "not_initialized" || status.SupervisorControl.NextAction != "none" {
		t.Fatalf("passive final Native status lost bootstrap-on-dispatch: exit=%d root_matches=%t control=%+v err=%v", code, status.RootID == f.rootID, status.SupervisorControl, err)
	}
}

func assertGatewayCurrentInfrastructureAbsent(t *testing.T, ctx context.Context, f *runtimeRestartCLINative) {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	_, observeErr := reader.InfrastructureRecordNamesContext(ctx)
	closeErr := reader.Close()
	if closeErr != nil || !errors.Is(observeErr, os.ErrNotExist) {
		t.Fatalf("uninitialized upgrade provisioned a Native owner journal: observation=%v close=%v", observeErr, closeErr)
	}
}

func newGatewayCurrentAdmissionRoot(t *testing.T, installation *runtimeRestartCLINative, client, daemon string) *runtimeRestartCLINative {
	t.Helper()
	f := newRuntimeRestartCLINative(t, client, daemon)
	selectGatewayInspectionUpgradeKeg(f, installation.bin)
	store, err := taskdir.InitStoreWithPredicatesContext(t.Context(), f.root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	f.rootID = store.RootID
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	f.pinRoot(t)
	// A regression may unexpectedly initialize a resident/daemon. Arm the
	// existing runtime cleanup before the common fixture can delete its root;
	// unresolved native closure retains it. Deliberately malformed private files
	// alone are inert and do not grant runtime cleanup authority.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		reader, openErr := taskdir.OpenReadOnlyStoreContext(ctx, f.root, predicate.Default())
		if openErr != nil {
			f.started = true
			t.Errorf("uninitialized fixture cleanup observation unavailable: %v", openErr)
			return
		}
		_, observeErr := reader.InfrastructureRecordNamesContext(ctx)
		closeErr := reader.Close()
		if closeErr != nil || !errors.Is(observeErr, os.ErrNotExist) {
			f.started = true
			closeQuietColdUpgradeFixture(t, f)
			if closeErr != nil {
				t.Error(closeErr)
			}
		}
	})
	return f
}

func seedGatewayCurrentPrivateState(t *testing.T, f *runtimeRestartCLINative, kind string) {
	t.Helper()
	private := filepath.Join(f.root, ".supervisor")
	switch kind {
	case "absent":
		return
	case "malformed_journal":
		if err := os.Mkdir(private, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(private, "bootstrap.pending"), []byte("{invalid retained bootstrap"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "wrong_type":
		if err := os.WriteFile(private, []byte("private directory type fault"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "symlink":
		if err := os.Symlink(filepath.Join(f.base, "home"), private); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown private-state fixture")
	}
}

func requireGatewayCurrentPrivateFault(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, token, kind string) {
	t.Helper()
	if kind == "malformed_journal" {
		store, err := taskdir.OpenExistingStoreMetadataContext(ctx, f.root, predicate.Default())
		if err != nil {
			t.Fatal(err)
		}
		pin, pinErr := store.PinRootContext(ctx)
		_, _, observeErr := pueue.ObservePrivateUpgrade(ctx, pin, "")
		closeErr := store.Close()
		var syntax *json.SyntaxError
		if pinErr != nil || closeErr != nil || !errors.Is(observeErr, pueue.ErrBinding) || !errors.As(observeErr, &syntax) {
			t.Fatalf("malformed journal lost its canonical raw decode fault: pin=%v close=%v observation=%v", pinErr, closeErr, observeErr)
		}
		return
	}
	maintenance := gatewayNativeMaintenance{dependencies: Dependencies{Environment: f.environment}.withCapturedEnvironmentContext(ctx).normalized()}
	observed, err := maintenance.Ensure(ctx, gatewaycommand.NativeMaintenanceRequest{Root: f.root, ExpectedRoot: token})
	if !errors.Is(err, pueue.ErrBinding) || observed != (gatewaycommand.NativeTransition{}) {
		t.Fatalf("non-directory private state was treated as absence or lost its raw fault: observed=%+v err=%v", observed, err)
	}
}

func runGatewayCurrentAdmissionRefusal(t *testing.T, ctx context.Context, f *runtimeRestartCLINative, profile *gatewayUpgradeNativeProfile, services *gatewayUpgradeNativeServices) {
	t.Helper()
	dependencies := Dependencies{
		Gateway:     &gatewaycommand.Dependencies{Client: profile, Executable: profile.selected, Services: services, Environment: f.environment},
		Environment: f.environment, RunnerExecutable: filepath.Join(f.bin, "delegate-run"), InitialSupervisorExecutable: f.originalClient,
	}
	gui := newGatewayGUIFixture(t, ctx, f.base, dependencies, profile)
	dependencies.Gateway.GUIUpgrade = gui.manager
	gui.dependencies = dependencies
	var output, diagnostic bytes.Buffer
	code := RunContext(ctx, []string{"gateway", "upgrade", "--root", f.root, "--profile", profile.directory, "--accept-execution-policy-removal", "--json"}, &output, &diagnostic, dependencies)
	gui.observeFinishedInvocation()
	var response gatewaycommand.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || code != 1 || response.Upgrade == nil || response.Failure == "" || response.Upgrade.Supervisor != (gatewaycommand.NativeTransition{}) {
		t.Fatalf("invalid private state did not remain a hard public failure: exit=%d failure=%s decode_error=%t", code, response.Failure, err != nil)
	}
}

type gatewayCurrentPrivateEntry struct {
	mode fs.FileMode
	data string
}

func gatewayCurrentPrivateState(t *testing.T, root string) map[string]gatewayCurrentPrivateEntry {
	t.Helper()
	entries := make(map[string]gatewayCurrentPrivateEntry)
	private := filepath.Join(root, ".supervisor")
	err := filepath.WalkDir(private, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == private && errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := gatewayCurrentPrivateEntry{mode: info.Mode()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			value.data, err = os.Readlink(path)
		case info.Mode().IsRegular() && info.Mode().Perm()&0o111 == 0:
			var data []byte
			data, err = os.ReadFile(path)
			value.data = string(data)
		case info.IsDir(), info.Mode()&os.ModeSocket != 0:
		default:
			return os.ErrInvalid
		}
		entries[path] = value
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
