package browser

import (
	"slices"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestBrowserActivationHasNoShellSpawnFallback(t *testing.T) {
	uri := "https://issuer.example/activate"
	executable, args, err := command("darwin", uri)
	if err != nil || executable != "/usr/bin/open" || !slices.Equal(args, []string{uri}) {
		t.Fatalf("unexpected LaunchServices request: %q %v %v", executable, args, err)
	}
	for _, platform := range []string{"linux", "windows"} {
		if executable, args, err = command(platform, uri); err == nil || executable != "" || len(args) != 0 {
			t.Fatalf("unsupported platform gained process fallback: %q %v", executable, args)
		}
	}
	if _, _, err = command("darwin", "file:///private"); err == nil {
		t.Fatal("unverified scheme accepted")
	}
}

func TestPortalAddressRequiresOneExplicitLocalTransport(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"unix:path=/run/user/1000/bus", "/run/user/1000/bus"},
		{"unix:path=/tmp/space%20bus,guid=0123456789abcdef0123456789abcdef", "/tmp/space bus"},
		{"unix:abstract=private-name", "@private-name"},
	} {
		got, err := socketAddress([]string{"DBUS_SESSION_BUS_ADDRESS=" + tc.raw})
		if err != nil || got != tc.want {
			t.Fatalf("local address %q: got%q err%v", tc.raw, got, err)
		}
	}
	for _, raw := range []string{"", "autolaunch:", "tcp:host=localhost,port=123", "unix:path=relative", "unix:path=/tmp/bus;unix:path=/other", "unix:path=/tmp/bus,path=/other", "unix:path=/tmp/bus,abstract=other", "unix:path=/tmp/bus,guid=bad", "unix:path=/tmp/%00bus", "unix:path=/tmp/%zz", "unix:abstract=" + strings.Repeat("a", 108)} {
		if _, err := socketAddress([]string{"DBUS_SESSION_BUS_ADDRESS=" + raw}); err == nil {
			t.Fatalf("unsafe bus address accepted: %q", raw)
		}
	}
}

func TestPortalSignalInboxFiltersAndStaysBounded(t *testing.T) {
	s := newPortalSignals()
	s.selectRequest(":1.7", "/expected")
	for _, signal := range []*dbus.Signal{
		{Sender: ":1.8", Path: "/expected", Name: responseSignal},
		{Sender: ":1.7", Path: "/other", Name: responseSignal},
		{Sender: ":1.7", Path: "/expected", Name: "other.Response"},
	} {
		s.DeliverSignal("", "", signal)
	}
	if len(s.response) != 0 {
		t.Fatal("foreign signal entered response inbox")
	}
	for range 1000 {
		s.DeliverSignal("", "", &dbus.Signal{Sender: ":1.7", Path: "/expected", Name: responseSignal})
	}
	if len(s.response) != 1 {
		t.Fatal("response inbox is not bounded to the first response")
	}
	s.Terminate()
	s.Terminate()
	s.DeliverSignal("", "", &dbus.Signal{Sender: ":1.7", Path: "/expected", Name: responseSignal})
}

func TestPortalResponseRequiresTypedSuccess(t *testing.T) {
	for _, body := range [][]any{nil, {uint32(0)}, {"0", map[string]dbus.Variant{}}, {uint32(0), "wrong"}, {uint32(3), map[string]dbus.Variant{}}, {uint32(1), map[string]dbus.Variant{}}, {uint32(2), map[string]dbus.Variant{}}} {
		if _, err := responseResult(&dbus.Signal{Body: body}); err == nil {
			t.Fatalf("invalid or cancelled response accepted: %#v", body)
		}
	}
	if complete, err := responseResult(&dbus.Signal{Body: []any{uint32(0), map[string]dbus.Variant{}}}); err != nil || !complete {
		t.Fatalf("typed successful response rejected: %v", err)
	}
}
