package infrastructure

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// The stop envelope evolves independently of preparation wire versions. This
// fixture exercises an authenticated native connection, not process closure.
func TestOwnerStopProtocolIsAcceptedBeforePreparationVersion(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.start()
	subject := f.request(t, ownerStatus)
	packet := struct {
		StopProtocol      int              `json:"stop_protocol"`
		RootID            string           `json:"root_id"`
		Owner             StartupReference `json:"owner"`
		PublicationSHA256 string           `json:"publication_sha256"`
		Client            ProcessIdentity  `json:"client"`
		Deadline          string           `json:"deadline"`
	}{1, subject.RootID, subject.Owner, subject.PublicationSHA256, subject.Client, subject.Deadline}
	data, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	connection := f.dial(t)
	if _, err = connection.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	wire, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("stable stop rejected as preparation protocol: %v", err)
	}
	var reply struct {
		StopProtocol      int              `json:"stop_protocol"`
		RootID            string           `json:"root_id"`
		Owner             StartupReference `json:"owner"`
		PublicationSHA256 string           `json:"publication_sha256"`
		Code              string           `json:"code"`
	}
	if err = json.Unmarshal(wire, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.StopProtocol != 1 || reply.RootID != subject.RootID || reply.Owner != subject.Owner || reply.PublicationSHA256 != subject.PublicationSHA256 || reply.Code != "accepted" {
		t.Fatalf("stop acknowledgement changed its authenticated subject: %+v", reply)
	}
	if !f.server.isClosing() {
		t.Fatal("accepted stop left new operation admission open")
	}
}

func stopServerFixture(t *testing.T, f *ownerServerFixture) ownerStopReply {
	t.Helper()
	request := ownerStopServerRequest(t, f)
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply, err := readOwnerFrame[ownerStopReply](bufio.NewReader(connection))
	if err != nil {
		t.Fatalf("stop reply failed: EOF=%t err=%v", errors.Is(err, io.EOF), err)
	}
	if reply.RootID != request.RootID || reply.Owner != request.Owner || reply.PublicationSHA256 != request.PublicationSHA256 {
		t.Fatal("stop changed authenticated subject")
	}
	return reply
}

func ownerStopServerRequest(t *testing.T, f *ownerServerFixture) ownerStopRequest {
	t.Helper()
	subject := f.request(t, ownerStatus)
	return ownerStopRequest{StopProtocol: 1, RootID: subject.RootID, Owner: subject.Owner, PublicationSHA256: subject.PublicationSHA256, Client: subject.Client, Deadline: subject.Deadline}
}

func TestOwnerStopBusyPreservesServingUntilInitialCompletes(t *testing.T) {
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(ctx context.Context, _ Operation, _ LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		entered <- ctx
		<-release
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	_, done, join := startOwnerRuntimeTest(t, f)
	t.Cleanup(unblock)
	awaitOwnerServerHost(t, entered)
	if reply := stopServerFixture(t, f); reply.Code != ownerStopBusy || f.server.isClosing() {
		t.Fatalf("busy stop changed serving admission: %+v", reply)
	}
	unblock()
	select {
	case <-f.server.initialDone:
	case <-time.After(5 * time.Second):
		t.Fatal("initial preparation did not settle")
	}
	if reply := stopServerFixture(t, f); reply.Code != ownerStopAccepted {
		t.Fatalf("settled stop refused: %+v", reply)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted stop did not drain")
	}
	if err := join(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerStopBeforeInitialRegistrationRemainsBusy(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	code := f.server.commitOwnerStop()
	if code != ownerStopBusy || f.server.isClosing() {
		t.Fatal("unregistered initial preparation lost serving admission")
	}
}

func TestOwnerStopKeepsLiveWaitForRuntimeCollection(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	defer close(done)
	if err := f.server.waits.RegisterDaemonWait(f.root, os.Getpid(), done); err != nil {
		t.Fatal(err)
	}
	f.start()
	reply := stopServerFixture(t, f)
	if reply.Code != ownerStopAccepted {
		t.Fatalf("stop discarded actual wait custody: %+v", reply)
	}
	assertOwnerStopRetainsDaemonWait(t, f.server, done)
	select {
	case <-done:
		t.Fatal("stop synthesized native Wait completion")
	default:
	}
}

func TestOwnerStopWireRejectsUnknownPreparationAndExecutableInputs(t *testing.T) {
	subject := ownerRequestFixture()
	request := ownerStopRequest{StopProtocol: 1, RootID: subject.RootID, Owner: subject.Owner, PublicationSHA256: subject.PublicationSHA256, Client: subject.Client, Deadline: subject.Deadline}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"runner":"/untrusted/program"`, `"arguments":["--untrusted"]`, `"protocol":3`, `"stop_protocol":1`, `"environment":[]`} {
		input := append([]byte{'{'}, data[1:]...)
		input = append(input[:len(input)-1], []byte(","+field+"}\n")...)
		_, stop, readErr := readOwnerControlFrame(bufio.NewReader(bytes.NewReader(input)))
		if readErr == nil || stop != (ownerStopRequest{}) {
			t.Fatalf("stop accepted unknown/duplicate field %s: %v", field, readErr)
		}
	}
}
