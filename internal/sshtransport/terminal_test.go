package sshtransport

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// terminalClient exercises actual SSH channel EOF, status and close ordering.
func terminalClient(t *testing.T, serve func(ssh.Channel, <-chan *ssh.Request)) *Client {
	t.Helper()
	host := key(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var client *Client
	t.Cleanup(func() {
		if client != nil {
			if closeErr := client.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if closeErr := listener.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		<-done
	})
	go func() {
		defer close(done)
		serveTerminalPeer(listener, host, serve)
	}()
	socket, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	connection, channels, requests, err := ssh.NewClientConn(socket, listener.Addr().String(), &ssh.ClientConfig{User: "fixture", HostKeyCallback: ssh.FixedHostKey(host.PublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	client = &Client{conn: ssh.NewClient(connection, channels, requests), executable: "delegate remote serve --stdio"}

	return client
}

func serveTerminalPeer(listener net.Listener, host ssh.Signer, serve func(ssh.Channel, <-chan *ssh.Request)) {
	socket, acceptErr := listener.Accept()
	if acceptErr != nil {
		return
	}
	defer closeConnection(socket)
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(host)
	connection, channels, requests, handshakeErr := ssh.NewServerConn(socket, config)
	if handshakeErr != nil {
		return
	}
	defer func() {
		if closeErr := connection.Close(); closeErr != nil {
			return
		}
	}()
	go ssh.DiscardRequests(requests)
	incoming, ok := <-channels
	if !ok {
		return
	}
	stream, requests, acceptErr := incoming.Accept()
	if acceptErr != nil {
		return
	}
	request, ok := <-requests
	if !ok || request.Type != "exec" {
		return
	}
	if replyErr := request.Reply(true, nil); replyErr != nil {
		return
	}
	serve(stream, requests)
}

func sendExit(t *testing.T, stream ssh.Channel, status uint32) {
	t.Helper()
	if _, err := stream.Stderr().Write([]byte("SECRET_REMOTE_STDERR")); err != nil {
		t.Error(err)
		return
	}
	if _, err := stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status})); err != nil {
		t.Error(err)
		return
	}
	if err := stream.Close(); err != nil {
		t.Error(err)
	}
}

func TestTerminalExitClassification(t *testing.T) {
	for _, test := range []struct {
		name   string
		status uint32
		code   string
	}{
		{"missing", 127, "remote_delegate_missing"},
		{"not-executable", 126, "remote_delegate_not_executable"},
		{"failed", 2, "remote_endpoint_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := terminalClient(t, func(stream ssh.Channel, _ <-chan *ssh.Request) { sendExit(t, stream, test.status) })
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			stream, err := client.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var content [1]byte
			_, err = stream.Read(content[:])
			var classified *Error
			if !errors.As(err, &classified) || classified.Code != test.code {
				t.Fatalf("wrong terminal classification: %v", err)
			}
			if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "SECRET_REMOTE_STDERR") {
				t.Fatalf("unsafe terminal error: %v", err)
			}
			// A second terminal operation must reuse Wait rather than deadlock waiting
			// for x/crypto's one-consumer exit-status result.
			if _, writeErr := stream.Write([]byte("after-exit")); writeErr == nil {
				t.Fatal("closed stream write succeeded")
			}
			if closeErr := stream.Close(); closeErr == nil || strings.Contains(closeErr.Error(), "SECRET_REMOTE_STDERR") {
				t.Fatalf("exit cleanup result lost or unsafe: %v", closeErr)
			}
		})
	}
}

func TestTerminalEOFWaitHonorsContext(t *testing.T) {
	eof := make(chan struct{})
	client := terminalClient(t, func(stream ssh.Channel, requests <-chan *ssh.Request) {
		if err := stream.CloseWrite(); err != nil {
			t.Error(err)
			return
		}
		close(eof)
		for range requests {
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Millisecond)
	defer cancel()
	stream, err := client.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-eof
	var data [1]byte
	if _, err = stream.Read(data[:]); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EOF-only peer ignored deadline: %v", err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalWaitConcurrentClose(t *testing.T) {
	eof := make(chan struct{})
	client := terminalClient(t, func(stream ssh.Channel, requests <-chan *ssh.Request) {
		if err := stream.CloseWrite(); err != nil {
			t.Error(err)
			return
		}
		close(eof)
		for range requests {
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stream, err := client.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { var data [1]byte; _, readErr := stream.Read(data[:]); finished <- readErr }()
	<-eof
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-finished:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("local close erased EOF: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("concurrent close did not release Wait")
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Open(ctx); err == nil {
		t.Fatal("client admitted a second exchange")
	}
}

func TestFullResponseDoesNotWaitForEndpointExit(t *testing.T) {
	client := terminalClient(t, func(stream ssh.Channel, requests <-chan *ssh.Request) {
		if _, err := stream.Write([]byte("response")); err != nil {
			t.Error(err)
			return
		}
		for range requests {
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stream, err := client.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	content := make([]byte, len("response"))
	if _, err = io.ReadFull(stream, content); err != nil || string(content) != "response" {
		t.Fatalf("response: %q %v", content, err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSingleExchangeOpenIsAtomic(t *testing.T) {
	client := terminalClient(t, func(_ ssh.Channel, requests <-chan *ssh.Request) {
		for range requests {
		}
	})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.Open(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open: %v", err)
	}
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	type opened struct {
		stream io.ReadWriteCloser
		err    error
	}
	results := make(chan opened, 2)
	for range 2 {
		go func() { stream, err := client.Open(ctx); results <- opened{stream, err} }()
	}
	successes := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			continue
		}
		successes++
		if err := result.stream.Close(); err != nil {
			t.Error(err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent open admitted %d exchanges", successes)
	}
}

func TestTerminalWithoutFailedExitPreservesEOF(t *testing.T) {
	for _, status := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-status", true: "zero-status"}[status], func(t *testing.T) {
			client := terminalClient(t, func(stream ssh.Channel, _ <-chan *ssh.Request) {
				if status {
					sendExit(t, stream, 0)
					return
				}
				if err := stream.Close(); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			stream, err := client.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var data [1]byte
			if _, err = stream.Read(data[:]); !errors.Is(err, io.EOF) {
				t.Fatalf("unclassified EOF lost: %v", err)
			}
			if err = stream.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFullResponsePrecedesFailedExit(t *testing.T) {
	client := terminalClient(t, func(stream ssh.Channel, _ <-chan *ssh.Request) {
		if _, err := stream.Write([]byte("response")); err != nil {
			t.Error(err)
			return
		}
		sendExit(t, stream, 2)
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stream, err := client.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	content := make([]byte, len("response"))
	if _, err = io.ReadFull(stream, content); err != nil || string(content) != "response" {
		t.Fatalf("full response erased by exit: %q %v", content, err)
	}
	// Teardown may race the final exit status. Any observed failed exit belongs
	// to cleanup; the completed response above remains available to its caller.
	if err = stream.Close(); err != nil {
		var classified *Error
		if !errors.As(err, &classified) || classified.Code != "remote_endpoint_failed" {
			t.Fatalf("unexpected cleanup failure: %v", err)
		}
	}
}
