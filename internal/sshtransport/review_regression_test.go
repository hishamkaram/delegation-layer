package sshtransport

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sys/unix"
)

func trustLock(t *testing.T) func(context.Context, func() error) error {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.OpenRemoteTrustContext(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := errors.Join(journal.Close(), store.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return func(ctx context.Context, action func() error) error {
		return journal.WithLock(ctx, func(_ *taskdir.ControlTransaction) error { return action() })
	}
}

func TestEnrollmentRejectsUnsafeTrustEntries(t *testing.T) {
	host := key(t)
	connection, dir := server(t, host)
	external := filepath.Join(dir, "external")
	if err := os.WriteFile(external, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	trust := filepath.Join(dir, "known_hosts")
	if err := os.Symlink(external, trust); err != nil {
		t.Fatal(err)
	}
	options := Options{KnownHostsFile: trust, WithTrustLock: trustLock(t)}
	if err := Enroll(context.Background(), connection, options, ssh.FingerprintSHA256(host.PublicKey())); err == nil {
		t.Fatal("enrollment followed trust symlink")
	}
	content, err := os.ReadFile(external)
	if err != nil || string(content) != "preserve\n" {
		t.Fatalf("external data changed: %q %v", content, err)
	}
	if err = os.Remove(trust); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(trust, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(trust, 0o666); err != nil {
		t.Fatal(err)
	}
	if err = Enroll(context.Background(), connection, options, ssh.FingerprintSHA256(host.PublicKey())); err == nil {
		t.Fatal("enrollment accepted writable trust file")
	}
}

func TestConcurrentEnrollmentRechecksUnderOwnerLock(t *testing.T) {
	host := key(t)
	connection, dir := server(t, host)
	options := Options{KnownHostsFile: filepath.Join(dir, "known_hosts"), WithTrustLock: trustLock(t)}
	ctx := context.Background()
	keys := []ssh.PublicKey{host.PublicKey(), key(t).PublicKey()}
	results := make(chan error, 2)
	for _, candidate := range keys {
		go func() {
			results <- options.WithTrustLock(ctx, func() error { return commitTrust(connection, options, candidate) })
		}()
	}
	failures := 0
	for range 2 {
		err := <-results
		if err != nil {
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "host_key_changed" {
				t.Fatal(err)
			}
			failures++
		}
	}
	content, err := os.ReadFile(options.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	if failures != 1 || strings.Count(string(content), "\n") != 1 {
		t.Fatalf("conflicting enrollment was not serialized: failures=%d bytes=%q", failures, content)
	}
}

// Native temporary roots avoid t.TempDir's test-name suffix exceeding macOS's
// Unix socket pathname bound. Listener cleanups register later and run first.
func shortAgentSocket(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "da-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			t.Error(removeErr)
		}
	})
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(canonical, "s")
	if len(path) >= len(unix.RawSockaddrUnix{}.Path) {
		t.Fatal("native temporary socket path exceeds sockaddr_un pathname bound")
	}
	return path
}

func TestAgentListHonorsContextCancellation(t *testing.T) {
	path := shortAgentSocket(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		socket, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		if _, readErr := io.Copy(io.Discard, socket); readErr != nil {
			return
		}
		closeConnection(socket)
	}()
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		<-done
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, socket, err := agentSigners(ctx, path, time.Second)
	if socket != nil {
		closeConnection(socket)
	}
	if err == nil {
		t.Fatal("silent agent list succeeded")
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("agent list exceeded caller deadline")
	}
}

func TestCertificateAuthorityPrefersHostCertificate(t *testing.T) {
	ca, host := key(t), key(t)
	certificate := &ssh.Certificate{Key: host.PublicKey(), CertType: ssh.HostCert, ValidPrincipals: []string{"127.0.0.1"}, ValidBefore: ssh.CertTimeInfinity}
	if err := certificate.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	certSigner, err := ssh.NewCertSigner(certificate, host)
	if err != nil {
		t.Fatal(err)
	}
	connection, dir := server(t, host, certSigner)
	trust := filepath.Join(dir, "known_hosts")
	line := "@cert-authority " + knownhosts.Line([]string{address(connection)}, ca.PublicKey()) + "\n"
	if err = os.WriteFile(trust, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, connection, Options{KnownHostsFile: trust})
	if err != nil {
		t.Fatal(err)
	}
	if client.Fingerprint() != ssh.FingerprintSHA256(host.PublicKey()) {
		t.Error("CA entry did not preserve the certified host key identity")
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
}

// stalledSigningAgent returns its identity normally, then leaves a signature
// request pending until the caller's deadline. This exercises authentication,
// not merely cancellation while listing agent identities.
type stalledSigningAgent struct {
	agent.Agent
	ctx       context.Context
	requested chan struct{}
}

func (a *stalledSigningAgent) Sign(_ ssh.PublicKey, _ []byte) (*ssh.Signature, error) {
	close(a.requested)
	<-a.ctx.Done()
	return nil, a.ctx.Err()
}

func TestAgentSigningHonorsHandshakeContext(t *testing.T) {
	host := key(t)
	connection, dir := server(t, host)
	content, err := os.ReadFile(connection.IdentityFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	private, err := ssh.ParseRawPrivateKey(content)
	if err != nil {
		t.Fatal(err)
	}
	ring := agent.NewKeyring()
	if err = ring.Add(agent.AddedKey{PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	stalled := &stalledSigningAgent{Agent: ring, ctx: ctx, requested: make(chan struct{})}
	path := shortAgentSocket(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		socket, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer closeConnection(socket)
		if serveErr := agent.ServeAgent(stalled, socket); serveErr != nil {
			return
		}
	}()
	t.Cleanup(func() {
		cancel()
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		<-done
	})
	connection.IdentityAgent = path
	connection.IdentityFiles = nil
	trust := filepath.Join(dir, "known_hosts")
	if err = os.WriteFile(trust, []byte(knownhosts.Line([]string{address(connection)}, host.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	client, err := Dial(ctx, connection, Options{KnownHostsFile: trust})
	if client != nil {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if err == nil {
		t.Fatal("stalled agent unexpectedly authenticated")
	}
	select {
	case <-stalled.requested:
	default:
		t.Fatal("test never reached signature request")
	}
	if time.Since(started) > time.Second {
		t.Fatal("signature request exceeded caller deadline")
	}
}

func TestAuthorityEntryRejectsUnavailableOrChangedReference(t *testing.T) {
	expected := key(t).PublicKey()
	other := key(t).PublicKey()
	line := "@cert-authority " + knownhosts.Line([]string{"fixture"}, expected) + "\n"
	for _, test := range []struct {
		name, content string
		referenceLine int
	}{{"missing", line, 2}, {"changed", knownhosts.Line([]string{"fixture"}, other) + "\n", 1}, {"beyond-limit", strings.Repeat("#\n", 2*1024*1024+1) + line, 2*1024*1024 + 2}} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "known_hosts")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			authority, err := authorityEntry(knownhosts.KnownKey{Key: expected, Filename: path, Line: test.referenceLine})
			if err == nil || authority {
				t.Fatalf("unverified reference accepted: authority=%v error=%v", authority, err)
			}
		})
	}
}

func hostCertificate(t *testing.T, authority, host ssh.Signer, serial uint64) *ssh.Certificate {
	t.Helper()
	certificate := &ssh.Certificate{Key: host.PublicKey(), Serial: serial, CertType: ssh.HostCert, ValidPrincipals: []string{"127.0.0.1"}, ValidBefore: ssh.CertTimeInfinity}
	if err := certificate.SignCert(rand.Reader, authority); err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestHostCertificateRenewalPreservesPinnedHostIdentity(t *testing.T) {
	authority, host := key(t), key(t)
	original := hostCertificate(t, authority, host, 1)
	signer, err := ssh.NewCertSigner(original, host)
	if err != nil {
		t.Fatal(err)
	}
	connection, dir := server(t, signer)
	trust := filepath.Join(dir, "known_hosts")
	line := "@cert-authority " + knownhosts.Line([]string{address(connection)}, authority.PublicKey()) + "\n"
	if err = os.WriteFile(trust, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := Dial(ctx, connection, Options{KnownHostsFile: trust})
	if err != nil {
		t.Fatal(err)
	}
	observed := client.Fingerprint()
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	expected := ssh.FingerprintSHA256(host.PublicKey())
	if observed != expected {
		t.Fatalf("certificate metadata leaked into durable host identity: got %s want %s", observed, expected)
	}
	if enrollErr := Enroll(ctx, connection, Options{KnownHostsFile: trust, WithTrustLock: trustLock(t)}, expected); enrollErr != nil {
		t.Fatalf("certified host enrollment did not use underlying identity: %v", enrollErr)
	}
	afterEnrollment, readErr := os.ReadFile(trust)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(afterEnrollment) != line {
		t.Fatal("enrollment rewrote configured authority trust")
	}
	verify, err := hostCallback(connection, Options{KnownHostsFile: trust, PinnedHostKey: observed})
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: connection.Port}
	renewed := hostCertificate(t, authority, host, 2)
	if err = verify(address(connection), remote, renewed); err != nil {
		t.Fatalf("same host key renewal rejected: %v", err)
	}
	replaced := hostCertificate(t, authority, key(t), 3)
	err = verify(address(connection), remote, replaced)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "host_key_identity_mismatch" {
		t.Fatalf("different certified host key accepted: %v", err)
	}
}

func TestUnknownHostWithCertificateEnrollsExplicitBareKey(t *testing.T) {
	authority, host := key(t), key(t)
	certificate := hostCertificate(t, authority, host, 1)
	signer, err := ssh.NewCertSigner(certificate, host)
	if err != nil {
		t.Fatal(err)
	}
	connection, dir := server(t, host, signer)
	trust := filepath.Join(dir, "known_hosts")
	expected := ssh.FingerprintSHA256(host.PublicKey())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	options := Options{KnownHostsFile: trust, WithTrustLock: trustLock(t)}
	if err = Enroll(ctx, connection, options, expected); err != nil {
		t.Fatalf("explicit bare-key enrollment failed when server also offers certificate: %v", err)
	}
	data, err := os.ReadFile(trust)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != knownhosts.Line([]string{address(connection)}, host.PublicKey())+"\n" {
		t.Fatalf("enrollment did not store exactly the bare host key: %s", data)
	}
	verify, err := hostCallback(connection, Options{KnownHostsFile: trust, PinnedHostKey: expected})
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: connection.Port}
	if err = verify(address(connection), remote, host.PublicKey()); err != nil {
		t.Fatalf("enrolled bare key rejected: %v", err)
	}
	if err = verify(address(connection), remote, certificate); err == nil {
		t.Fatal("bare-key enrollment unexpectedly granted certificate authority trust")
	}
}
