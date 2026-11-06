package infrastructure

import (
	"testing"
	"time"
)

func TestOwnerServerEmptyProbeDoesNotConsumeDiagnostic(t *testing.T) {
	for _, payload := range []string{"", "{", "not-json\n"} {
		t.Run(map[string]string{"": "empty", "{": "partial", "not-json\n": "malformed"}[payload], func(t *testing.T) {
			exerciseOwnerEmptyProbe(t, payload)
		})
	}
}

func exerciseOwnerEmptyProbe(t *testing.T, payload string) {
	t.Helper()
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	client := f.dial(t)
	if err := f.server.listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	server, err := f.server.listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.server.finishConnection(server) })
	if !f.server.registerConnection(server) {
		t.Fatal("probe not registered")
	}
	if payload != "" {
		if _, err = client.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err = client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	f.server.handleConnection(t.Context(), server)
	if diagnostic := f.server.diagnostic(); (diagnostic != nil) != (payload != "") {
		t.Fatalf("payload=%q diagnostic=%v", payload, diagnostic)
	}
}
