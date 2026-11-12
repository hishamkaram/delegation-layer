package host

import (
	"bufio"
	"bytes"
	"testing"
	"time"
)

func TestHostWireStatusPreservesNativeRetryTimestamp(t *testing.T) {
	instant := time.Date(2026, time.November, 1, 13, 14, 15, 123, time.UTC)
	want := Status{
		State: "backoff", Epoch: "0123456789abcdef0123456789abcdef", Attempt: 7,
		NextRetryAt: &instant, Protocol: ProtocolVersion, BuildID: "build", Host: Identity{PID: 42, StartToken: "exact"},
	}
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, wireResponse{Status: statusWire(want)}); err != nil {
		t.Fatal(err)
	}
	var received wireResponse
	if err := readFrame(bufio.NewReader(&buffer), &received); err != nil {
		t.Fatal(err)
	}
	status, err := received.Status.value()
	if err != nil || status.NextRetryAt == nil || !status.NextRetryAt.Equal(instant) || status.Attempt != want.Attempt {
		t.Fatalf("retry status changed across private IPC: %+v %v", status, err)
	}
	received.Status.NextRetryAt = "invalid timestamp"
	if _, err = received.Status.value(); err == nil {
		t.Fatal("untrusted retry timestamp accepted")
	}
}

func TestHostWireDecodeRetainsLifecycleReadinessWithoutInstallationReadiness(t *testing.T) {
	wire := statusWire(Status{State: "ready", HostReady: true})
	if !wire.Ready {
		t.Fatal("legacy status lost lifecycle readiness")
	}
	status, err := wire.value()
	if err != nil || !status.HostReady || status.Ready || status.Health != nil {
		t.Fatalf("unverified wire reply claimed public readiness: %+v %v", status, err)
	}
	observed := applyInstallationHealth(status, InstallationHealth{State: "available"})
	if !observed.HostReady || !observed.Ready {
		t.Fatalf("available installation lost lifecycle readiness: %+v", observed)
	}
}
