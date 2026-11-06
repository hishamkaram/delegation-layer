package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sys/unix"
)

func stopLocal(t *testing.T, query Query, done <-chan error) {
	t.Helper()
	epoch, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("live epoch: %v %t", err, present)
	}
	response, err := contact(t.Context(), query, epoch, "stop")
	if err != nil || !response.Accepted || !response.Status.StopAccepted {
		t.Fatalf("stop acknowledgement=%+v err=%v", response, err)
	}
	if err = receive(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestHostPermanentChildFaultDoesNotRelaunchAndKeepsControls(t *testing.T) {
	for _, mode := range []string{"permanent", "runtime_permanent"} {
		t.Run(mode, func(t *testing.T) {
			config := testConfig(t, mode)
			clock := clockwork.NewFakeClock()
			done := runLocalHost(t, config, clock)
			query := queryFor(t, config)
			status := eventuallyStatus(t, query, func(status Status) bool {
				return status.State == "degraded" && status.LastError == "incompatible_definition" && status.Child == nil
			})
			if status.Attempt != 1 || status.Ready || status.NextRetryAt != nil {
				t.Fatalf("permanent fault scheduled another child: %+v", status)
			}
			clock.Advance(24 * time.Hour)
			status, err := Observe(t.Context(), query)
			if err != nil || status.Attempt != 1 || status.LastError != "incompatible_definition" {
				t.Fatalf("permanent fault lost after elapsed jump: %+v %v", status, err)
			}
			stopLocal(t, query, done)
		})
	}
}

func TestHostPreflightFinishesBeforeReadyAndPermanentRefusal(t *testing.T) {
	config := testConfig(t, "gated_permanent")
	markers := t.TempDir()
	config.Environment = append(config.Environment, "DL_HOST_MARKERS="+markers)
	clock := clockwork.NewFakeClock()
	done := runLocalHost(t, config, clock)
	query := queryFor(t, config)
	eventuallyStatus(t, query, func(status Status) bool {
		_, err := os.Stat(filepath.Join(markers, "preflight-entered"))
		return err == nil && status.Child != nil
	})
	before, err := Observe(t.Context(), query)
	if err != nil || before.Ready || before.State != "starting" {
		t.Fatalf("Ready preceded preflight: %+v %v", before, err)
	}
	if err = os.WriteFile(filepath.Join(markers, "preflight-release"), []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := eventuallyStatus(t, query, func(status Status) bool { return status.LastError == "incompatible_definition" && status.Child == nil })
	clock.Advance(24 * time.Hour)
	after, err := Observe(t.Context(), query)
	if err != nil || status.Ready || after.Ready || after.Attempt != 1 || after.NextRetryAt != nil {
		t.Fatalf("preflight permanent fault relaunched: %+v %v", after, err)
	}
	stopLocal(t, query, done)
}

func TestHostRetriesOnlyAfterWaitWithExactIndefiniteCap(t *testing.T) {
	config := testConfig(t, "crash")
	clock := clockwork.NewFakeClock()
	done := runLocalHost(t, config, clock)
	query := queryFor(t, config)
	for index, delay := range []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, time.Minute,
		2 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	} {
		attempt := uint64(index + 1)
		status := eventuallyStatus(t, query, func(status Status) bool {
			return status.Attempt == attempt && status.State == "backoff" && status.Child == nil
		})
		want := clock.Now().Add(delay).UTC()
		if status.NextRetryAt == nil || !status.NextRetryAt.Equal(want) {
			t.Fatalf("attempt %d retry=%v want=%s", attempt, status.NextRetryAt, want)
		}
		if index == 9 {
			break
		}
		clock.Advance(delay - time.Nanosecond)
		before, err := Observe(t.Context(), query)
		if err != nil || before.Attempt != attempt {
			t.Fatalf("child started before cooldown: %+v %v", before, err)
		}
		clock.Advance(time.Nanosecond)
	}
	stopLocal(t, query, done)
	clock.Advance(24 * time.Hour)
	status, err := Observe(t.Context(), query)
	if err != nil || status.State != "no_host_observed" {
		t.Fatalf("stop restarted host: %+v %v", status, err)
	}
}

func TestHostLiveUnreadyIsNeverReplacedAndStopBeforeReadyIsClean(t *testing.T) {
	config := testConfig(t, "unready")
	clock := clockwork.NewFakeClock()
	done := runLocalHost(t, config, clock)
	query := queryFor(t, config)
	started := eventuallyStatus(t, query, func(status Status) bool { return status.Attempt == 1 && status.Child != nil })
	clock.Advance(10 * time.Second)
	status := eventuallyStatus(t, query, func(status Status) bool { return status.LastError == "readiness_timeout" })
	clock.Advance(24 * time.Hour)
	after, err := Observe(t.Context(), query)
	if err != nil || after.Attempt != 1 || after.Ready || after.Child == nil || *after.Child != *started.Child || status.Ready {
		t.Fatalf("live-unready child replaced: %+v %v", after, err)
	}
	stopLocal(t, query, done)
}

func TestHostWaitsForPriorRuntimeWithoutFalseReady(t *testing.T) {
	config := testConfig(t, "hold")
	execution, err := scheduler.AcquireOwnerLease(t.Context(), config.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := execution.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	clock := clockwork.NewFakeClock()
	done := runLocalHost(t, config, clock)
	query := queryFor(t, config)
	status := eventuallyStatus(t, query, func(status Status) bool {
		return status.LastError == "waiting_for_prior_runtime" && status.Child == nil
	})
	if status.Ready || status.State != "starting" {
		t.Fatalf("prior E was treated as ready/crashed: %+v", status)
	}
	if err = execution.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	ready := eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	if ready.Attempt != 2 {
		t.Fatalf("unexpected replacement attempts: %+v", ready)
	}
	stopLocal(t, query, done)
}

func TestHostPublicStopJoinsChildAndReleasesHAndE(t *testing.T) {
	config := testConfig(t, "hold")
	process := startHostFixture(t, config)
	query := queryFor(t, config)
	eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := Stop(ctx, query)
	if err != nil || !result.Accepted || !result.Stopped || result.Status.State != "stopped" {
		t.Fatalf("public stop=%+v err=%v", result, err)
	}
	receive(t, process.done)
	if process.err != nil {
		t.Fatal(process.err)
	}
	assertLeasesFree(t, config)
}

func assertLeasesFree(t *testing.T, config Config) {
	t.Helper()
	host, err := AcquireHostLease(t.Context(), config.Store)
	if err != nil {
		t.Fatalf("H was not released: %v", err)
	}
	defer func() {
		if closeErr := host.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	execution, err := scheduler.AcquireOwnerLease(t.Context(), config.Store)
	if err != nil {
		t.Fatalf("E was not released: %v", err)
	}
	if err = execution.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHostAbruptLossStopsChildOnEOFAndRetainsExplicitContext(t *testing.T) {
	config := testConfig(t, "fd_probe")
	markers := t.TempDir()
	config.Directory = markers
	config.Environment = append(config.Environment, "DL_HOST_MARKERS="+markers, "DL_HOST_VISIBLE=")
	process := startHostFixture(t, config)
	query := queryFor(t, config)
	ready := eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	if ready.Child == nil {
		t.Fatal("ready without a child identity")
	}
	epoch, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("host endpoint: %t %v", present, err)
	}
	pinLostHostEndpoint(t, process, epoch.Endpoint)
	assertCapturedContext(t, markers, ready.Host.PID)
	if _, writeErr := process.control.Write([]byte{'x'}); writeErr != nil {
		t.Fatal(writeErr)
	}
	receive(t, process.done)
	if process.err == nil {
		t.Fatal("abrupt host fixture did not exit abnormally")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if waitErr := awaitIdentityAbsent(ctx, *ready.Child); waitErr != nil {
		t.Fatalf("child retained E after host EOF: %v", waitErr)
	}
	assertLeasesFree(t, config)
	status, err := Observe(t.Context(), query)
	if err != nil || status.State != "no_host_observed" || status.Ready {
		t.Fatalf("lost host was declared stopped/ready: %+v %v", status, err)
	}
}

func assertCapturedContext(t *testing.T, markers string, hostPID int) {
	t.Helper()
	contextFile := filepath.Join(markers, "context")
	var content []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var readErr error
		content, readErr = os.ReadFile(contextFile)
		if readErr == nil {
			break
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal(readErr)
		}
		timer := time.NewTimer(time.Millisecond)
		<-timer.C
	}
	sid, err := unix.Getsid(hostPID)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(content), "\n")
	expectedCwd, err := filepath.EvalSymlinks(markers)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 4 || parts[0] != "true" || parts[1] != "" || parts[2] != expectedCwd {
		t.Fatalf("captured context not preserved: %q", content)
	}
	childSID, err := strconv.Atoi(parts[3])
	if err != nil || childSID == sid {
		t.Fatalf("scheduler child shares host session: host=%d child=%q err=%v", sid, parts[3], err)
	}
}
