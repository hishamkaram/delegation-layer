package sshtransport

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func key(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func server(t *testing.T, host ssh.Signer, extra ...ssh.Signer) (sshconfig.Connection, string) {
	t.Helper()
	return serverChannel(t, host, nil, extra...)
}

func serverChannel(t *testing.T, host ssh.Signer, serve func(*testing.T, ssh.Channel, <-chan *ssh.Request), extra ...ssh.Signer) (sshconfig.Connection, string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, pub ssh.PublicKey) (*ssh.Permissions, error) {
		if string(pub.Marshal()) != string(clientKey.PublicKey().Marshal()) {
			return nil, errors.New("wrong key")
		}
		return nil, nil
	}}
	config.AddHostKey(host)
	for _, signer := range extra {
		config.AddHostKey(signer)
	}
	fixture := &sshFixture{listener: listener, config: config, done: make(chan struct{}), serveChannel: serve}
	go fixture.serve(t)
	t.Cleanup(func() { fixture.close(t) })
	hostName, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if modeErr := os.Chmod(dir, 0o700); modeErr != nil {
		t.Fatal(modeErr)
	}
	canonicalDir, canonicalErr := filepath.EvalSymlinks(dir)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	dir = canonicalDir
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "identity")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return sshconfig.Connection{Alias: "fixture", HostName: hostName, Port: port, User: "fixture", IdentityAgent: "none", IdentityFiles: []string{keyPath}}, dir
}

type sshFixture struct {
	serveChannel func(*testing.T, ssh.Channel, <-chan *ssh.Request)
	listener     net.Listener
	config       *ssh.ServerConfig
	workers      sync.WaitGroup
	connections  sync.Map
	done         chan struct{}
}

func (f *sshFixture) serve(t *testing.T) {
	defer close(f.done)
	for {
		socket, acceptErr := f.listener.Accept()
		if acceptErr != nil {
			return
		}
		f.connections.Store(socket, socket)
		f.workers.Add(1)
		go f.connection(t, socket)
	}
}

func (f *sshFixture) connection(t *testing.T, socket net.Conn) {
	defer f.workers.Done()
	defer f.connections.Delete(socket)
	defer closeConnection(socket)
	conn, channels, requests, handshakeErr := ssh.NewServerConn(socket, f.config)
	if handshakeErr != nil {
		return
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			return
		}
	}()
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		channel, channelRequests, acceptErr := incoming.Accept()
		if acceptErr != nil {
			return
		}
		if f.serveChannel != nil {
			f.serveChannel(t, channel, channelRequests)
		} else {
			f.channel(t, channel, channelRequests)
		}
	}
}

func (f *sshFixture) channel(t *testing.T, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() {
		if closeErr := channel.Close(); closeErr != nil {
			return
		}
	}()
	for request := range requests {
		if request.Type != "exec" {
			if replyErr := request.Reply(false, nil); replyErr != nil {
				return
			}
			continue
		}
		var command struct{ Command string }
		if decodeErr := ssh.Unmarshal(request.Payload, &command); decodeErr != nil {
			return
		}
		if command.Command != "delegate remote serve --stdio" {
			t.Errorf("unexpected executable text %q", command.Command)
		}
		if replyErr := request.Reply(true, nil); replyErr != nil {
			return
		}
		if _, copyErr := io.Copy(channel, channel); copyErr != nil {
			return
		}
		return
	}
}

func (f *sshFixture) close(t *testing.T) {
	t.Helper()
	if closeErr := f.listener.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	<-f.done
	f.connections.Range(func(_, value any) bool {
		socket, ok := value.(net.Conn)
		if !ok {
			t.Error("invalid connection")
			return false
		}
		if closeErr := socket.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
		return true
	})
	f.workers.Wait()
}

func TestVerifiedChannelAndTrust(t *testing.T) {
	host := key(t)
	connection, dir := server(t, host)
	trust := filepath.Join(dir, "known_hosts")
	options := Options{KnownHostsFile: trust, WithTrustLock: trustLock(t)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Dial(ctx, connection, options)
	var classified *Error
	if !errors.As(err, &classified) || classified.Code != "host_key_unknown" {
		t.Fatalf("unknown host accepted or misclassified: %v", err)
	}
	if enrollErr := Enroll(ctx, connection, options, "SHA256:wrong"); enrollErr == nil {
		t.Fatal("wrong fingerprint enrolled")
	}
	if enrollErr := Enroll(ctx, connection, options, ssh.FingerprintSHA256(host.PublicKey())); enrollErr != nil {
		t.Fatal(enrollErr)
	}
	client, err := Dial(ctx, connection, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	channel, err := client.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := channel.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 5)
	if _, err := io.ReadFull(channel, content); err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello" {
		t.Fatal(string(content))
	}
	if err := channel.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestChangedAndRevokedHosts(t *testing.T) {
	host := key(t)
	connection, dir := server(t, host)
	trust := filepath.Join(dir, "known_hosts")
	other := key(t)
	for _, test := range []struct{ line, code string }{{knownhosts.Line([]string{address(connection)}, other.PublicKey()), "host_key_changed"}, {"@revoked " + knownhosts.Line([]string{address(connection)}, host.PublicKey()), "host_key_revoked"}} {
		if err := os.WriteFile(trust, []byte(test.line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := Dial(ctx, connection, Options{KnownHostsFile: trust})
		cancel()
		var classified *Error
		if !errors.As(err, &classified) || classified.Code != test.code {
			t.Fatalf("got %v, want %s", err, test.code)
		}
	}
}

func TestExecutableRejectsShellText(t *testing.T) {
	for _, path := range []string{"relative/delegate", "/bin/delegate; touch /tmp/no", "/path with spaces/delegate"} {
		if _, err := invocation(path); err == nil {
			t.Fatalf("unsafe executable accepted %q", path)
		}
	}
}

func TestKnownHostAlgorithmPreference(t *testing.T) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	known, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	connection, dir := server(t, key(t), known)
	connection.HostKeyAlias = "trusted-alias"
	trust := filepath.Join(dir, "known_hosts")
	line := knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize(verificationAddress(connection)))}, known.PublicKey())
	if err = os.WriteFile(trust, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, connection, Options{KnownHostsFile: trust})
	if err != nil {
		t.Fatal(err)
	}
	if client.Fingerprint() != ssh.FingerprintSHA256(known.PublicKey()) {
		t.Error("negotiated an untrusted host-key algorithm")
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestChannelContextClosesBlockedRead(t *testing.T) {
	host := key(t)
	connection, dir := server(t, host)
	trust := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(trust, []byte(knownhosts.Line([]string{address(connection)}, host.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, connection, Options{KnownHostsFile: trust})
	if err != nil {
		t.Fatal(err)
	}
	channelCtx, channelCancel := context.WithCancel(ctx)
	stream, err := client.Open(channelCtx)
	if err != nil {
		t.Fatal(err)
	}
	channelCancel()
	finished := make(chan error, 1)
	go func() { var b [1]byte; _, readErr := stream.Read(b[:]); finished <- readErr }()
	select {
	case readErr := <-finished:
		if readErr == nil {
			t.Fatal("closed channel read succeeded")
		}
	case <-ctx.Done():
		t.Fatal("channel read ignored cancellation")
	}
	if closeErr := stream.Close(); closeErr != nil && !errors.Is(closeErr, io.EOF) {
		t.Error(closeErr)
	}
	if closeErr := client.Close(); closeErr != nil {
		t.Error(closeErr)
	}
}
