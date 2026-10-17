//go:build darwin

package lifetime

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	darwinNestedHelperMode = "DELEGATE_DARWIN_NESTED_HELPER"
	darwinNestedPIDFile    = "DELEGATE_DARWIN_NESTED_PID_FILE"
)

func TestMain(m *testing.M) {
	if code, handled := RunProcessGroupPinMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestDarwinProcessGroupRegistrationUsesPlanAndPrivateSession(t *testing.T) {
	plan := processGroupPlan("root", "task", "darwin-session-registration")
	scope, err := CreatePrepared(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.BindOwnerProcess(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })

	ref := scope.Reference()
	if ref.Identifier() != plan.Identifier() {
		t.Fatalf("registered Darwin scope identifier = %q, want plan %q", ref.Identifier(), plan.Identifier())
	}
	reopened, err := OpenRegistered(plan, ref)
	if err != nil {
		t.Fatalf("OpenRegistered session scope: %v", err)
	}
	t.Cleanup(func() {
		if releaseErr := reopened.Release(); releaseErr != nil {
			t.Errorf("reopened scope Release: %v", releaseErr)
		}
	})
	cmd := exec.Command("sleep", "60")
	if err = reopened.Configure(cmd); err != nil {
		t.Fatalf("Configure through pinned registration: %v", err)
	}
	implementation, ok := reopened.impl.(*processGroupScope)
	if !ok {
		t.Fatal("reopened scope is not a process-group scope")
	}
	if implementation.pin != nil {
		t.Fatal("new Darwin scope retained a process-group pin")
	}
	assertDarwinCommandStartsPrivateSession(t, scope, cmd)
}

func TestDarwinProcessGroupRejectsNumericSignalAuthority(t *testing.T) {
	if err := signalProcessGroup(4343, syscall.SIGTERM); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("numeric process-group signal error = %v, want ErrUnsupported", err)
	}
}

func TestDarwinProcessGroupAllowsVerifiedNestedSignalTargets(t *testing.T) {
	ref, err := NewProcessRef(4242, "darwin:1:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProcessGroupSignalTargets(&processGroupScope{}, map[int][]LaunchRef{4343: nil}); err != nil {
		t.Fatalf("nested target validation = %v, want nil", err)
	}
	if ref.PID() == 0 {
		t.Fatal("test identity was not constructed")
	}
}

func TestDarwinProcessGroupPinReferenceRejectsChangedPlan(t *testing.T) {
	ref, err := NewProcessRef(4242, "darwin:1:1")
	if err != nil {
		t.Fatal(err)
	}
	pin := &darwinProcessGroupPin{
		ref: ref, address: "127.0.0.1:4242",
		token: strings.Repeat("a", 64),
	}
	first := processGroupPlan("root", "task", "first")
	second := processGroupPlan("root", "task", "second")
	identifier := encodeDarwinProcessGroupPinReference(first.Identifier(), pin)
	base, _, err := decodeDarwinProcessGroupPinReference(identifier)
	if err != nil || base != first.Identifier() {
		t.Fatalf("pin reference round trip = (%q, %v)", base, err)
	}
	dynamic, err := NewRef(KindProcessGroup, AssuranceCooperative, identifier)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenRegistered(second, dynamic); err == nil {
		t.Fatal("OpenRegistered accepted a Darwin pin from another plan")
	}
}

func TestDarwinProcessGroupPinRequestCancellationClosesConnectedSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	assertDarwinProcessGroupPinRequestInterrupted(t, ctx, cancel, context.Canceled)
}

func TestDarwinProcessGroupPinRequestDeadlineClosesConnectedSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	assertDarwinProcessGroupPinRequestInterrupted(t, ctx, func() {}, context.DeadlineExceeded)
}

func assertDarwinProcessGroupPinRequestInterrupted(
	t *testing.T,
	ctx context.Context,
	interrupt func(),
	want error,
) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Errorf("close test listener: %v", closeErr)
		}
	})
	ref, err := NewProcessRef(4242, "darwin:1:1")
	if err != nil {
		t.Fatal(err)
	}
	pin := &darwinProcessGroupPin{
		ref: ref, address: listener.Addr().String(), token: strings.Repeat("a", 64),
	}
	requestDone := make(chan error, 1)
	go func() { requestDone <- pin.requestUnchecked(ctx, processGroupPinProbe) }()

	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := connection.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Errorf("close accepted test connection: %v", closeErr)
		}
	})
	if err = connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = bufio.NewReader(connection).ReadString('\n'); err != nil {
		t.Fatalf("read pin request: %v", err)
	}
	if err = ctx.Err(); err != nil {
		t.Fatalf("request context ended before the connected-socket assertion: %v", err)
	}
	interrupt()
	select {
	case requestErr := <-requestDone:
		if !errors.Is(requestErr, want) {
			t.Fatalf("interrupted pin request error = %v, want %v", requestErr, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled pin request did not release the connected socket")
	}
}
