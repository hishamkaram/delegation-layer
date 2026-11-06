package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestOwnerStopFullControlCapacityReturnsAuthenticatedBusy(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	controls := holdOwnerStopControlCapacity(t, f)
	f.start()
	if reply := stopServerFixture(t, f); reply.Code != ownerStopBusy {
		t.Fatalf("full control capacity did not return authenticated busy: %+v", reply)
	}
	if f.server.isClosing() {
		t.Fatal("full control capacity stopped serving admission")
	}
	if err := f.entry.validateResident(t.Context()); err != nil {
		t.Fatalf("busy stop changed resident ownership: %v", err)
	}
	if diagnostic := f.server.diagnostic(); diagnostic != nil {
		t.Fatalf("ordinary capacity refusal became an operational fault: %v", diagnostic)
	}
	f.server.mu.Lock()
	controlCount := f.server.connectionCount(ownerConnectionControl)
	f.server.mu.Unlock()
	if controlCount != ownerControlLimit {
		t.Fatalf("busy stop changed admitted control slots: %d", controlCount)
	}
	for _, connection := range controls {
		f.server.finishConnection(connection)
	}
	if reply := stopServerFixture(t, f); reply.Code != ownerStopAccepted {
		t.Fatalf("released control capacity did not admit stop: %+v", reply)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.server.stopAcceptAndDrain(ctx); err != nil {
		t.Fatalf("stop did not drain its actual connections: %v", err)
	}
}

func TestOwnerStopFullControlCapacityAuthenticatesBeforeBusy(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*ownerStopRequest)
	}{
		{"root", func(request *ownerStopRequest) { request.RootID = strings.Repeat("b", 32) }},
		{"owner", func(request *ownerStopRequest) { request.Owner.Sequence++ }},
		{"publication", func(request *ownerStopRequest) { request.PublicationSHA256 = strings.Repeat("c", 64) }},
		{"peer", func(request *ownerStopRequest) { request.Client.PID++ }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newOwnerServerFixture(t, &ownerServerTestHost{})
			if _, err := f.server.prepareInitial(t.Context()); err != nil {
				t.Fatal(err)
			}
			holdOwnerStopControlCapacity(t, f)
			request := ownerStopServerRequest(t, f)
			change.mutate(&request)
			f.start()
			connection := f.dial(t)
			if err := writeOwnerFrame(connection, request); err != nil {
				t.Fatal(err)
			}
			if reply, err := readOwnerFrame[ownerStopReply](bufio.NewReader(connection)); !errors.Is(err, io.EOF) {
				t.Fatalf("unauthenticated capacity request received a reply: %+v err=%v", reply, err)
			}
			if f.server.isClosing() || f.server.diagnostic() == nil {
				t.Fatal("authentication refusal changed serving state or lost its diagnostic")
			}
		})
	}
}

// Each retained slot has a real Unix connection, native peer authentication and
// the production store's exact publication subject. Fixture cleanup owns their
// closure even if the request or an assertion fails before explicit release.
func holdOwnerStopControlCapacity(t *testing.T, f *ownerServerFixture) []*net.UnixConn {
	t.Helper()
	request := f.request(t, ownerStatus)
	var controls []*net.UnixConn
	for range ownerControlLimit {
		connection := manuallyAcceptOwnerServerConnection(t, f)
		if !f.server.registerConnection(connection) {
			t.Fatal("control fixture registration refused before capacity")
		}
		if _, err := f.server.acceptWork(t.Context(), connection, request); err != nil {
			t.Fatalf("authenticated control refused before capacity: %v", err)
		}
		controls = append(controls, connection)
	}
	return controls
}
