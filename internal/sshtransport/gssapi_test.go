package sshtransport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func gssapiFixture(t *testing.T) (sshconfig.Connection, Options) {
	t.Helper()
	host := key(t)
	connection, directory := server(t, host)
	connection.GSSAPIAuthentication = true
	trust := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(trust, []byte(knownhosts.Line([]string{address(connection)}, host.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return connection, Options{KnownHostsFile: trust, Environment: []string{}}
}

func TestGSSAPISuccessDiagnosticRequiresSelectedMethod(t *testing.T) {
	for _, test := range []struct {
		name      string
		preferred []string
		want      []Diagnostic
	}{
		{"default", nil, []Diagnostic{{Code: "gssapi_unavailable"}}},
		{"excluded", []string{"publickey"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection, options := gssapiFixture(t)
			connection.PreferredAuthentications = test.preferred
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client, err := Dial(ctx, connection, options)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(client.Diagnostics(), test.want) {
				t.Errorf("diagnostics=%+v, want=%+v", client.Diagnostics(), test.want)
			}
			if err = client.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type gssapiFailureCase struct {
	name, setup, code string
	preferred         []string
	unavailable       bool
}

func TestGSSAPIAuthenticationFailuresRemainTyped(t *testing.T) {
	for _, test := range []gssapiFailureCase{
		{"missing-key", "empty", "authentication_unavailable", nil, true},
		{"missing-agent", "agent", "agent_unavailable", nil, true},
		{"rejected-key", "rejected", "authentication_failed", nil, true},
		{"gssapi-excluded", "rejected", "authentication_failed", []string{"publickey"}, false},
	} {
		t.Run(test.name, func(t *testing.T) { checkGSSAPIAuthenticationFailure(t, test) })
	}
}

func checkGSSAPIAuthenticationFailure(t *testing.T, test gssapiFailureCase) {
	t.Helper()
	connection, options := gssapiFixture(t)
	connection.PreferredAuthentications = test.preferred
	switch test.setup {
	case "empty":
		connection.IdentityFiles = nil
	case "agent":
		connection.IdentityAgent = filepath.Join(filepath.Dir(options.KnownHostsFile), "missing-agent")
	case "rejected":
		replaceAuthenticationKey(t, connection.IdentityFiles[0])
	default:
		t.Fatalf("unknown fixture setup %q", test.setup)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := Dial(ctx, connection, options)
	var classified *Error
	if !errors.As(err, &classified) || classified.Code != test.code || classified.GSSAPIUnavailable != test.unavailable {
		t.Fatalf("authentication classification=%+v error=%v", classified, err)
	}
}

func replaceAuthenticationKey(t *testing.T, path string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}
