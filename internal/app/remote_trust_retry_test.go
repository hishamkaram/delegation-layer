package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"github.com/hishamkaram/delegation-layer/internal/sshtransport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestRemoteTrustRetryPolicy(t *testing.T) {
	for _, test := range []struct {
		code string
		want int
	}{
		{"unreachable", 5},
		{"host_key_enrollment_failed", 5},
		{"host_key_fingerprint_mismatch", 1},
		{"host_key_changed", 1},
		{"host_key_revoked", 1},
		{"authentication_failed", 1},
		{"configuration", 1},
		{"local_commit", 1},
	} {
		t.Run(test.code, func(t *testing.T) {
			failure := errors.Join(&sshtransport.Error{Code: test.code}, io.EOF)
			calls := 0
			var delays []time.Duration
			policy := remote.RetryPolicy{Sleep: func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }}
			started := time.Now()
			err := retryRemoteEnrollment(t.Context(), policy, func(ctx context.Context) error {
				calls++
				deadline, finite := ctx.Deadline()
				if !finite || deadline.After(started.Add(remote.DefaultOperationTimeout+time.Second)) {
					t.Fatal("attempt lacks finite operation bound")
				}
				return failure
			})
			if !errors.Is(err, io.EOF) || calls != test.want {
				t.Fatalf("failure=%v attempts=%d want%d", err, calls, test.want)
			}
			expected := []time.Duration(nil)
			if test.want == 5 {
				expected = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
			}
			if !reflect.DeepEqual(delays, expected) {
				t.Fatalf("retry delays=%v want%v", delays, expected)
			}
		})
	}
}

func TestRemoteTrustRetryStopsAtCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	policy := remote.RetryPolicy{Sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }}
	err := retryRemoteEnrollment(ctx, policy, func(context.Context) error {
		calls++
		return errors.Join(&sshtransport.Error{Code: "unreachable"}, io.EOF)
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancel did not stop retries: %v calls%d", err, calls)
	}
}

func TestRemoteTrustRetryDoesNotRetryUntypedOrLocalFailures(t *testing.T) {
	for _, failure := range []error{io.EOF, errors.New("local trust journal failed"), &sshtransport.Error{Code: "unreachable"}} {
		calls := 0
		err := retryRemoteEnrollment(t.Context(), remote.RetryPolicy{}, func(context.Context) error { calls++; return failure })
		if !errors.Is(err, failure) || calls != 1 {
			t.Fatalf("unclassified failure retried: %v calls%d", err, calls)
		}
	}
}

func TestRemoteTrustRecoversPreKeyEOFAndCommitsOnce(t *testing.T) {
	signer := remoteTrustSigner(t)
	listener, connections := startRemoteTrustServer(t, signer)
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "state")
	// The trust owner requires a canonical private parent on both Linux and macOS.
	canonicalParent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(canonicalParent, "state")
	trust := filepath.Join(root, "remote-known_hosts")
	connection := sshconfig.Connection{HostName: host, Port: port, User: "fixture"}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	options := sshtransport.Options{KnownHostsFile: trust}
	fingerprint := ssh.FingerprintSHA256(signer.PublicKey())
	if err = enrollRemoteDevice(ctx, connection, options, fingerprint, Dependencies{}, root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(trust)
	if err != nil {
		t.Fatal(err)
	}
	expected := knownhosts.Line([]string{listener.Addr().String()}, signer.PublicKey()) + "\n"
	if string(data) != expected || connections.Load() != 3 {
		t.Fatalf("enrollment attempts=%d trust=%q", connections.Load(), data)
	}
}

func remoteTrustSigner(t *testing.T) ssh.Signer {
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

func serveTrustEnrollment(t *testing.T, socket net.Conn, signer ssh.Signer) {
	t.Helper()
	defer func() { closeRemoteTrustConnection(t, socket.Close()) }()
	if err := socket.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Error(err)
		return
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	connection, _, _, err := ssh.NewServerConn(socket, config)
	if err != nil {
		t.Error(err)
		return
	}
	// Keep the authenticated server alive until enrollment closes its client;
	// racing server teardown against that close is a different failure boundary.
	closeRemoteTrustConnection(t, connection.Wait())
	closeRemoteTrustConnection(t, connection.Close())
}

func startRemoteTrustServer(t *testing.T, signer ssh.Signer) (net.Listener, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	connections := new(atomic.Int32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			socket, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if connections.Add(1) <= 2 {
				closeRemoteTrustConnection(t, socket.Close())
				continue
			}
			serveTrustEnrollment(t, socket, signer)
		}
	}()
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		<-done
	})
	return listener, connections
}

func closeRemoteTrustConnection(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
		t.Error(err)
	}
}

func TestRemoteTrustRetryStopsBeforeAttemptAtCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	calls := 0
	err := retryRemoteEnrollment(ctx, remote.RetryPolicy{}, func(context.Context) error { calls++; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
		t.Fatalf("expired caller admitted an attempt: %v calls%d", err, calls)
	}
}
