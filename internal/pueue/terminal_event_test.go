package pueue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRecordPrivateTerminalEventSkipsOrdinaryTaskWithoutTakingEventLock(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	lockPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventLockName)
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if err := RecordPrivateTerminalEvent(context.Background(), fixture.stateRoot, 41, "0"); err != nil {
		t.Fatalf("ordinary task callback required the event lock: %v", err)
	}
	eventPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventRecordDir, "41.json")
	if _, err := os.Lstat(eventPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary task callback created terminal state: %v", err)
	}
}

func TestReadPrivateTerminalEventDoesNotProvisionAbsentEventLock(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	lockPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, privateTerminalEventLockName)
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	if _, found, err := client.ReadPrivateTerminalEvent(context.Background(), 41); err != nil || found {
		t.Fatalf("read missing event found=%t err=%v", found, err)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read missing event provisioned callback lock: %v", err)
	}
}

func TestPrivateTerminalEventAPIsRejectNilContext(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	var nilContext context.Context

	if enabled, err := client.PrivateTerminalEventsEnabled(nilContext); !errors.Is(err, context.Canceled) || enabled {
		t.Fatalf("nil-context enabled returned enabled=%t err=%v, want context.Canceled", enabled, err)
	}
	if err := client.RegisterPrivateTerminalEvent(nilContext, 42); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-context register returned %v, want context.Canceled", err)
	}
	if event, found, err := client.ReadPrivateTerminalEvent(nilContext, 42); !errors.Is(err, context.Canceled) || found || event != (TerminalEvent{}) {
		t.Fatalf("nil-context read returned event=%+v found=%t err=%v, want context.Canceled", event, found, err)
	}
	if err := client.ClearPrivateTerminalEvents(nilContext, 42); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-context clear returned %v, want context.Canceled", err)
	}
	if err := RecordPrivateTerminalEvent(nilContext, fixture.stateRoot, 42, "0"); !errors.Is(err, context.Canceled) {
		t.Fatalf("nil-context callback returned %v, want context.Canceled", err)
	}
}

func TestPrivateTerminalEventTicketPublishesIdempotentlyAndClearsAfterProof(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	ctx := context.Background()
	assertPrivateTerminalEventTicketPublishes(t, fixture.stateRoot, client, ctx)
	assertPrivateTerminalEventTicketClears(t, fixture.stateRoot, client, ctx)
}

func assertPrivateTerminalEventTicketPublishes(t *testing.T, stateRoot string, client *Client, ctx context.Context) {
	t.Helper()
	if enabled, err := client.PrivateTerminalEventsEnabled(ctx); err != nil || !enabled {
		t.Fatalf("terminal events enabled=%t err=%v", enabled, err)
	}
	if err := client.RegisterPrivateTerminalEvent(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(ctx, stateRoot, 42, "0"); err != nil {
		t.Fatal(err)
	}
	if err := RecordPrivateTerminalEvent(ctx, stateRoot, 42, "0"); err != nil {
		t.Fatalf("same terminal callback was not idempotent: %v", err)
	}
	event, found, err := client.ReadPrivateTerminalEvent(ctx, 42)
	if err != nil || !found || !event.Succeeded || event.NumericTaskID != 42 {
		t.Fatalf("terminal event = %+v found=%t err=%v", event, found, err)
	}
	if err = RecordPrivateTerminalEvent(ctx, stateRoot, 42, "1"); !errors.Is(err, ErrBinding) {
		t.Fatalf("conflicting terminal callback error = %v, want binding error", err)
	}
}

func assertPrivateTerminalEventTicketClears(t *testing.T, stateRoot string, client *Client, ctx context.Context) {
	t.Helper()
	if err := client.ClearPrivateTerminalEvents(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, found, err := client.ReadPrivateTerminalEvent(ctx, 42); err != nil || found {
		t.Fatalf("cleared terminal event found=%t err=%v", found, err)
	}
	if err := RecordPrivateTerminalEvent(ctx, stateRoot, 42, "0"); err != nil {
		t.Fatalf("callback after ticket clear failed: %v", err)
	}
	if _, found, err := client.ReadPrivateTerminalEvent(ctx, 42); err != nil || found {
		t.Fatalf("callback recreated cleared event: found=%t err=%v", found, err)
	}
}

func TestTerminalEventExitCodeIsStrict(t *testing.T) {
	for _, tc := range []struct {
		code string
		want bool
		err  bool
	}{
		{code: "0", want: true},
		{code: "1"},
		{code: "-1"},
		{code: "None"},
		{code: "00", err: true},
		{code: "+1", err: true},
		{code: "1\n", err: true},
		{code: "", err: true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			got, err := terminalEventSucceeded(tc.code)
			if (err != nil) != tc.err || got != tc.want {
				t.Fatalf("terminalEventSucceeded(%q) = (%t, %v), want (%t, error=%t)", tc.code, got, err, tc.want, tc.err)
			}
		})
	}
}

func TestPrivateTerminalEventConfigRequiresExactCallback(t *testing.T) {
	base := filepath.Join(canonicalTemp(t), "state", privateSupervisorDirectory)
	config, err := ParseConfig([]byte(privateConfigYAML(base)))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveConfig(config, fixedResolution())
	if err != nil {
		t.Fatal(err)
	}
	if !privateTerminalEventsConfigured(base, resolved.Daemon.Callback) {
		t.Fatalf("generated private callback = %q, want exact event contract", valueOrEmpty(resolved.Daemon.Callback))
	}
	if err = resolved.ValidateIsolation(base); err != nil {
		t.Fatalf("generated private callback did not validate: %v", err)
	}
	callback := *resolved.Daemon.Callback + " unexpected"
	resolved.Daemon.Callback = &callback
	if err = resolved.ValidateIsolation(base); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("unexpected callback validated: %v", err)
	}
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
