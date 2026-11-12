package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/jonboulle/clockwork"
)

func installedRunnerFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "delegate-run")
	// Metadata-only fixture; it is never executed or read by the host.
	if err := os.WriteFile(path, []byte("fixture"), 0o100); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHostInstallationChangesPreserveLifecycleAndLegacyControls(t *testing.T) {
	for _, change := range []string{"missing", "replaced"} {
		t.Run(change, func(t *testing.T) {
			config := testConfig(t, "hold")
			config.RunnerExecutable = installedRunnerFixture(t)
			done := runLocalHost(t, config, clockwork.NewFakeClock())
			query := queryFor(t, config)
			before := eventuallyStatus(t, query, func(s Status) bool { return s.Ready })
			if !before.HostReady || before.Health == nil || before.Health.State != "available" {
				t.Fatalf("missing installation baseline: %+v", before)
			}
			reason := changeInstalledRunner(t, config.RunnerExecutable, change)
			after, err := Observe(t.Context(), query)
			assertInstallationDegraded(t, before, after, reason, err)
			assertLegacyHostReady(t, query)
			stopLocal(t, query, done)
			assertLeasesFree(t, config)
		})
	}
}

func TestInstallationObservationExcludesAccessTimeAndExecutableContents(t *testing.T) {
	path := installedRunnerFixture(t)
	baseline := observeInstallation([]installedFile{{path: path, role: "delegate_run"}}, os.Stat)
	if baseline.health.State != "available" {
		t.Fatal(baseline.health)
	}
	info := baseline.files[0].info
	if err := os.Chtimes(path, info.ModTime().Add(time.Hour), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	after := observeInstallation(baseline.files, os.Stat)
	if after.health.State != "available" {
		t.Fatalf("access time changed installation health: %+v", after.health)
	}
}

func TestHostStalledInstallationProbeKeepsControlsAndOwnsShutdown(t *testing.T) {
	config := testConfig(t, "hold")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// Register release after the host cleanup so LIFO cleanup unblocks I/O first.
	var calls atomic.Int32
	stat := func(path string) (os.FileInfo, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return os.Stat(path)
	}
	done := runLocalHostWithStat(t, config, clockwork.NewFakeClock(), stat)
	t.Cleanup(unblock)
	receive(t, entered)
	query := queryFor(t, config)
	observed := eventuallyStatus(t, query, func(s Status) bool { return s.HostReady })
	if observed.State != "degraded" || observed.Fault || observed.Ready || observed.Health == nil || observed.Health.State != "unknown" || observed.Health.Reason != "installation_probe_pending" {
		t.Fatalf("blocked probe claimed readiness: %+v", observed)
	}
	if calls.Load() != 1 {
		t.Fatalf("blocked probe spawned additional I/O: %d", calls.Load())
	}
	assertProbeStopPending(t, config, query)
	select {
	case hostErr := <-done:
		t.Fatalf("host abandoned owned probe: %v", hostErr)
	default:
	}
	unblock()
	if hostErr := receive(t, done); hostErr != nil {
		t.Fatal(hostErr)
	}
	assertLeasesFree(t, config)
}

func changeInstalledRunner(t *testing.T, path, change string) string {
	t.Helper()
	if change == "missing" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return "installation_missing"
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(filepath.Dir(path), "replacement")
	if err = os.WriteFile(replacement, []byte("fixture"), info.Mode()); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	return "installation_changed"
}

func assertLegacyHostReady(t *testing.T, query Query) {
	t.Helper()
	epoch, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("epoch: %t %v", present, err)
	}
	legacy, err := contact(t.Context(), query, epoch, "status")
	if err != nil || !legacy.Status.Ready || legacy.Status.State != "ready" {
		t.Fatalf("strict legacy status changed: %+v err=%v", legacy, err)
	}
}

func assertProbeStopPending(t *testing.T, config Config, query Query) {
	t.Helper()
	epoch, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("epoch: %t %v", present, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	response, err := contact(ctx, query, epoch, "stop")
	if err != nil || !response.Accepted {
		t.Fatalf("stop was blocked by installation I/O: %+v %v", response, err)
	}
	stopping, err := Observe(ctx, query)
	if err != nil || stopping.State != "stopping" || stopping.Ready || !stopping.StopAccepted || stopping.Health == nil || stopping.Health.State != "unknown" {
		t.Fatalf("pending shutdown was not control-responsive: %+v %v", stopping, err)
	}
	assertHostLeaseHeld(t, ctx, config)
}

func assertHostLeaseHeld(t *testing.T, ctx context.Context, config Config) {
	t.Helper()
	lease, leaseErr := AcquireHostLease(ctx, config.Store)
	if lease != nil {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("H released before probe join")
	}
	if !errors.Is(leaseErr, task.ErrLockBusy) {
		t.Fatalf("unexpected H observation: %v", leaseErr)
	}
}

func assertInstallationDegraded(t *testing.T, before, after Status, reason string, err error) {
	t.Helper()
	if err != nil || !after.HostReady || after.Ready || after.Fault || after.State != "degraded" || after.LastError != reason || after.Health == nil || after.Health.Role != "delegate_run" || after.Attempt != before.Attempt || after.Child == nil || *after.Child != *before.Child {
		t.Fatalf("installation change lost readiness or lifecycle distinction: %+v err=%v", after, err)
	}
}

func TestInstallationProbeRechecksCompletedPriorRequest(t *testing.T) {
	path := installedRunnerFixture(t)
	config := Config{Executable: path, RunnerExecutable: path}
	probe := newInstallationProbe(config, os.Stat)
	probe.files = observeInstallation(probe.files, os.Stat).files
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release, sampled := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	probe.stat = func(selected string) (os.FileInfo, error) {
		info, err := os.Stat(selected)
		switch calls.Add(1) {
		case 1:
			cancel()
			<-release
		case 2:
			close(sampled)
		}
		return info, err
	}
	owner := &service{installation: probe}
	t.Cleanup(owner.joinInstallationProbe)
	t.Cleanup(unblock)
	prior := owner.installationHealth(ctx)
	if prior.State != "unknown" {
		t.Fatalf("interrupted request claimed completed health: %+v", prior)
	}
	unblock()
	receive(t, sampled) // Both prior metadata samples precede replacement.
	changeInstalledRunner(t, path, "replaced")
	current := owner.installationHealth(t.Context())
	if current.State != "degraded" || current.Reason != "installation_changed" || calls.Load() != 4 {
		t.Fatalf("current request reused prior available result: %+v calls=%d", current, calls.Load())
	}
}

func TestInstallationHealthRejectsPrivateOrClientOnlyWireValues(t *testing.T) {
	for _, payload := range []InstallationHealth{
		{State: "available", Reason: "PRIVATE_VALUE"},
		{State: "available", Role: "PRIVATE_VALUE"},
		{State: "degraded", Reason: "PRIVATE_VALUE", Role: "delegate"},
		{State: "degraded", Reason: "installation_changed", Role: "PRIVATE_VALUE"},
		{State: "unknown", Reason: "PRIVATE_VALUE"},
		{State: "unknown", Reason: "installation_probe_pending", Role: "delegate"},
		{State: "unsupported", Reason: "installation_health_unsupported"},
	} {
		health, err := installationHealthFromResponse(wireHealthResponse{Health: &payload})
		projected := applyInstallationHealth(Status{State: "ready", HostReady: true}, health)
		if !errors.Is(err, ErrIdentity) || projected.Ready || projected.LastError != "installation_health_invalid" || health != (InstallationHealth{State: "unknown", Reason: "installation_health_invalid"}) {
			t.Fatalf("invalid private wire values were retained: projected=%+v health=%+v err=%v", projected, health, err)
		}
	}
}

func TestInstallationHealthAcceptsNativeWireCombinations(t *testing.T) {
	for _, payload := range []InstallationHealth{
		{State: "available"},
		{State: "degraded", Reason: "installation_missing", Role: "delegate"},
		{State: "degraded", Reason: "installation_changed", Role: "delegate_run"},
		{State: "degraded", Reason: "installation_unusable", Role: "delegate"},
		{State: "unknown", Reason: "installation_observation_incomplete", Role: "delegate"},
		{State: "unknown", Reason: "installation_baseline_unavailable", Role: "delegate_run"},
		{State: "unknown", Reason: "installation_probe_pending"},
		{State: "unknown", Reason: "installation_host_stopping"},
	} {
		health, err := installationHealthFromResponse(wireHealthResponse{Health: &payload})
		if err != nil || health != payload {
			t.Fatalf("native health combination refused: %+v %v", payload, err)
		}
	}
}
