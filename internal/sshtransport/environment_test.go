package sshtransport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestSelectEnvironmentBoundaries(t *testing.T) {
	c := sshconfig.Connection{SendEnv: []string{"*"}}
	entry := "K=" + strings.Repeat("v", 32766)
	if got, err := SelectEnvironment(c, []string{entry}); err != nil || len(got) != 1 || len(got[0].Value) != 32766 {
		t.Fatalf("exact entry boundary: %d %v", len(got), err)
	}
	var count, total []string
	for i := 0; i < 128; i++ {
		count = append(count, fmt.Sprintf("K%d=v", i))
	}
	for i := 0; i < 8; i++ {
		total = append(total, fmt.Sprintf("K%d=", i)+strings.Repeat("v", 32765))
	}
	for _, snapshot := range [][]string{count, total} {
		if _, err := SelectEnvironment(c, snapshot); err != nil {
			t.Fatalf("exact boundary rejected: %v", err)
		}
	}
	for _, snapshot := range [][]string{append(count, "EXTRA=v"), append(total, "EXTRA=v"), {entry + "v"}} {
		_, err := SelectEnvironment(c, snapshot)
		var classified *Error
		if !errors.As(err, &classified) || classified.Code != "ssh_environment_limit" {
			t.Fatalf("limit classification: %v", err)
		}
	}
}

func TestSelectEnvironmentMatchesNamesAndPreservesSnapshot(t *testing.T) {
	c := sshconfig.Connection{SendEnv: []string{"LANG", "LC_?", "TOKEN"}}
	snapshot := []string{"LANG=C.UTF-8", "LC_X=literal=value", "LC_LONG=unselected", "TOKEN=secret", "OTHER=TOKEN"}
	original := append([]string(nil), snapshot...)
	got, err := SelectEnvironment(c, snapshot)
	want := []EnvironmentVariable{{"LANG", "C.UTF-8"}, {"LC_X", "literal=value"}, {"TOKEN", "secret"}}
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(snapshot, original) {
		t.Fatalf("selection = %#v %v", got, err)
	}
	for _, snapshot := range [][]string{{"TOKEN=secret\x00value"}, {"TOKEN"}, {"TOKEN=one", "TOKEN=two"}} {
		_, err := SelectEnvironment(c, snapshot)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "TOKEN") {
			t.Fatalf("invalid selection/redaction: %v", err)
		}
	}
}

func TestPolicyAndEnvironmentFailBeforeDial(t *testing.T) {
	c := sshconfig.Connection{PubkeyAuthentication: "no", IdentityAgent: "/missing-agent"}
	_, err := Dial(t.Context(), c, Options{})
	var classified *Error
	if !errors.As(err, &classified) || classified.Code != "authentication_unavailable" {
		t.Fatalf("auth policy reached trust/agent/network: %v", err)
	}
	c = sshconfig.Connection{SendEnv: []string{"TOKEN"}, IdentityAgent: "/missing-agent"}
	_, err = Dial(t.Context(), c, Options{Environment: []string{"TOKEN=" + strings.Repeat("s", 32768)}})
	if !errors.As(err, &classified) || classified.Code != "ssh_environment_limit" {
		t.Fatalf("environment limit reached trust/agent/network: %v", err)
	}
}

type environmentRequestCase struct {
	name        string
	refuse      bool
	environment []string
	expected    []EnvironmentVariable
}

func TestSSHEnvironmentRequestsBeforeExec(t *testing.T) {
	t.Setenv("LANG", "ambient-not-selected")
	for _, test := range []environmentRequestCase{
		{"accept", false, []string{"LANG=C.UTF-8", "LC_CTYPE=C", "SECRET=not-forwarded"}, []EnvironmentVariable{{"LANG", "C.UTF-8"}, {"LC_CTYPE", "C"}}},
		{"refuse", true, []string{"LANG=C.UTF-8", "LC_CTYPE=C", "SECRET=not-forwarded"}, []EnvironmentVariable{{"LANG", "C.UTF-8"}, {"LC_CTYPE", "C"}}},
		{"captured-empty", false, []string{}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkEnvironmentRequests(t, test)
		})
	}
}

// environmentPeer records the request order and owns its completion signal.
type environmentPeer struct {
	refuse bool
	seen   chan []EnvironmentVariable
	done   chan struct{}
}

func (p *environmentPeer) serve(t *testing.T, ch ssh.Channel, requests <-chan *ssh.Request) {
	defer close(p.done)
	defer func() {
		if err := settledConnectionClose(ch.Close()); err != nil {
			t.Error(err)
		}
	}()
	var received []EnvironmentVariable
	for request := range requests {
		if request.Type == "env" {
			var variable EnvironmentVariable
			if err := ssh.Unmarshal(request.Payload, &variable); err != nil {
				t.Error(err)
				return
			}
			received = append(received, variable)
			if err := request.Reply(!p.refuse, nil); err != nil {
				t.Error(err)
				return
			}
			continue
		}
		if request.Type != "exec" {
			t.Errorf("unexpected request %s", request.Type)
			return
		}
		if err := request.Reply(true, nil); err != nil {
			t.Error(err)
			return
		}
		p.seen <- received
		if _, err := ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0})); err != nil {
			t.Error(err)
		}
		return
	}
}

func checkEnvironmentRequests(t *testing.T, test environmentRequestCase) {
	t.Helper()
	peer := &environmentPeer{refuse: test.refuse, seen: make(chan []EnvironmentVariable, 1), done: make(chan struct{})}
	host := key(t)
	connection, dir := serverChannel(t, host, peer.serve)
	connection.SendEnv = []string{"LANG", "LC_*"}
	connection.GSSAPIAuthentication = true
	trust := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(trust, []byte(knownhosts.Line([]string{address(connection)}, host.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, connection, Options{KnownHostsFile: trust, Environment: test.environment})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-peer.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	if got := <-peer.seen; !reflect.DeepEqual(got, test.expected) {
		t.Fatalf("requests = %#v", got)
	}
	want := []Diagnostic{{Code: "gssapi_unavailable"}}
	if test.refuse {
		want = append(want, Diagnostic{Code: "ssh_env_refused", Count: 2})
	}
	got := client.Diagnostics()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics = %#v", got)
	}
	got[0].Code = "mutated"
	if client.Diagnostics()[0].Code != "gssapi_unavailable" {
		t.Fatal("caller mutated observations")
	}
}

func TestSSHEnvironmentTransportFailureDoesNotExec(t *testing.T) {
	host := key(t)
	seen := make(chan string, 2)
	connection, dir := serverChannel(t, host, func(t *testing.T, ch ssh.Channel, requests <-chan *ssh.Request) {
		defer func() {
			if err := settledConnectionClose(ch.Close()); err != nil {
				t.Error(err)
			}
		}()
		request, ok := <-requests
		if ok {
			seen <- request.Type
		}
	})
	connection.SendEnv = []string{"LANG"}
	trust := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(trust, []byte(knownhosts.Line([]string{address(connection)}, host.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	client, err := Dial(ctx, connection, Options{KnownHostsFile: trust, Environment: []string{"LANG=C"}})
	if err != nil {
		t.Fatal(err)
	}
	if stream, err := client.Open(ctx); err == nil || stream != nil {
		t.Fatalf("lost environment acknowledgment succeeded: %v", err)
	}
	if got := <-seen; got != "env" {
		t.Fatalf("started command before environment request: %s", got)
	}
}
