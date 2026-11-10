package system

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func TestDeviceProbeActionUsesVerifiedControlledSSH(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	peer, config := startProbePeer(t, ctx)
	root := filepath.Join(t.TempDir(), "absent-state")
	remoteRoot := filepath.Join(t.TempDir(), "destination")
	backend := app.NewWorkbench(app.Dependencies{Environment: []string{}})
	action := findAction(t, NewSection(backend, app.Arguments{Root: root}, t.TempDir()).Actions, "devices-probe")
	request := prepareProbe(t, action, config, remoteRoot)
	feedback := action.Run(ctx, request)
	if feedback.Err != nil || !strings.Contains(feedback.Body, "Connectivity: connected") {
		t.Fatalf("UI probe did not reach verified endpoint: %+v", feedback)
	}
	select {
	case actual := <-peer.seen:
		if actual.Operation != remote.Hello || actual.Root != remoteRoot || actual.Provider != "" || actual.Cwd != "" {
			t.Fatalf("probe changed scope or requested provider work: %+v", actual)
		}
	case <-ctx.Done():
		t.Fatal("probe never reached controlled protocol peer:", ctx.Err())
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe initialized execution root: %v", err)
	}
}

func prepareProbe(t *testing.T, action ui.Action, config, root string) app.Arguments {
	t.Helper()
	values := make([]string, len(action.Fields))
	for i, field := range action.Fields {
		values[i] = field.Value
		switch field.Label {
		case "SSH config":
			values[i] = config
		case "SSH alias":
			values[i] = "ui-peer"
		case "Remote root":
			values[i] = root
		}
	}
	request, err := action.Prepare(values)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// probePeer owns one ephemeral SSH listener/connection and their joined worker.
// Keys/config are generated only for this fixture. It accepts one framed Hello,
// rejecting environment/agent/terminal requests or any provider operation.
type probePeer struct {
	mu       sync.Mutex
	listener net.Listener
	socket   net.Conn
	closing  bool
	config   *ssh.ServerConfig
	done     chan error
	seen     chan remote.Request
}

func startProbePeer(t *testing.T, ctx context.Context) (*probePeer, string) {
	t.Helper()
	host, _ := probeKey(t)
	client, private := probeKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer := &probePeer{listener: listener, done: make(chan error, 1), seen: make(chan remote.Request, 1)}
	peer.config = &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(key.Marshal(), client.PublicKey().Marshal()) {
			return nil, errors.New("unexpected test client key")
		}
		return nil, nil
	}}
	peer.config.AddHostKey(host)
	go func() { peer.done <- peer.serve(ctx) }()
	t.Cleanup(func() {
		if closeErr := peer.close(); closeErr != nil {
			t.Error(closeErr)
		}
		if serveErr := <-peer.done; serveErr != nil {
			t.Error("controlled SSH peer:", serveErr)
		}
	})
	return peer, probeConfiguration(t, listener.Addr().String(), host, private)
}

func probeKey(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer, private
}

func probeConfiguration(t *testing.T, address string, host ssh.Signer, private ed25519.PrivateKey) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	writeProbeFile(t, filepath.Join(directory, "identity"), pem.EncodeToMemory(block))
	writeProbeFile(t, filepath.Join(directory, "known_hosts"), []byte(knownhosts.Line([]string{knownhosts.Normalize(address)}, host.PublicKey())+"\n"))
	writeProbeFile(t, filepath.Join(directory, "empty_hosts"), nil)
	hostname, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf("Host ui-peer\n HostName %s\n Port %s\n User fixture\n IdentityFile %q\n IdentityAgent none\n IdentitiesOnly yes\n UserKnownHostsFile %q\n GlobalKnownHostsFile %q\n", hostname, port, filepath.Join(directory, "identity"), filepath.Join(directory, "known_hosts"), filepath.Join(directory, "empty_hosts"))
	path := filepath.Join(directory, "config")
	writeProbeFile(t, path, []byte(configuration))
	return path
}

func writeProbeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (p *probePeer) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closing = true
	err := settledProbeClose(p.listener.Close())
	if p.socket != nil {
		err = errors.Join(err, settledProbeClose(p.socket.Close()))
	}
	return err
}

func settledProbeClose(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (p *probePeer) serve(ctx context.Context) (result error) {
	socket, err := p.listener.Accept()
	if err != nil {
		return settledProbeClose(err)
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return settledProbeClose(socket.Close())
	}
	p.socket = socket
	p.mu.Unlock()
	defer func() { result = errors.Join(result, settledProbeClose(socket.Close())) }()
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("controlled probe requires an explicit deadline")
	}
	if err = socket.SetDeadline(deadline); err != nil {
		return err
	}
	connection, channels, requests, err := ssh.NewServerConn(socket, p.config)
	if err != nil {
		return err
	}
	discarded := make(chan struct{})
	go func() { defer close(discarded); ssh.DiscardRequests(requests) }()
	defer func() { result = errors.Join(result, settledProbeClose(connection.Close())); <-discarded }()
	incoming, ok := <-channels
	if !ok {
		return errors.New("probe did not open an SSH channel")
	}
	if incoming.ChannelType() != "session" {
		return errors.New("probe opened a non-session SSH channel")
	}
	channel, channelRequests, err := incoming.Accept()
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, settledProbeClose(channel.Close())) }()
	return p.exchange(channel, channelRequests)
}

func (p *probePeer) exchange(channel ssh.Channel, requests <-chan *ssh.Request) error {
	request, ok := <-requests
	if !ok || request.Type != "exec" {
		return errors.New("probe requested environment, agent, terminal or no exec")
	}
	var command struct{ Command string }
	if err := ssh.Unmarshal(request.Payload, &command); err != nil {
		return err
	}
	if command.Command != "delegate remote serve --stdio" {
		return errors.New("probe did not use installed framed endpoint")
	}
	if err := request.Reply(true, nil); err != nil {
		return err
	}
	frame := remote.Framer{Reader: channel, Writer: channel}
	actual, err := frame.ReadRequest()
	if err != nil {
		return err
	}
	if actual.Operation != remote.Hello || actual.Provider != "" {
		return errors.New("probe attempted a non-observation operation")
	}
	p.seen <- actual
	response := remote.Response{Version: remote.ProtocolVersion, Identity: remote.Identity{Root: actual.Root, RootID: strings.Repeat("c", 32)}}
	if err = frame.WriteResponse(response); err != nil {
		return err
	}
	_, err = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
	return err
}
