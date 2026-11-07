// Package sshtransport owns verified embedded SSH connections and exec channels.
// It never interprets task state or retries an execution.
package sshtransport

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Error classifies transport failure without exposing native authentication data.
type Error struct {
	Code        string
	Fingerprint string
	cause       error
}

func (e *Error) Error() string {
	if e.Fingerprint != "" {
		return e.Code + ": " + e.Fingerprint
	}
	return e.Code
}
func (e *Error) Unwrap() error { return e.cause }

// hostIdentityFingerprint names the host's signing key, independent of a CA
// certificate's serial, validity window or signature. Certificate authorization
// remains the known-hosts verifier's responsibility on every connection.
func hostIdentityFingerprint(key ssh.PublicKey) string {
	if certificate, ok := key.(*ssh.Certificate); ok {
		return ssh.FingerprintSHA256(certificate.Key)
	}
	return ssh.FingerprintSHA256(key)
}

// Options are caller-owned connection bounds and existing trust intent.
type Options struct {
	KnownHostsFile, PinnedHostKey, DelegateExecutable string
	DialTimeout, HandshakeTimeout                     time.Duration
	// WithTrustLock serializes enrollment through the caller-owned durable store.
	WithTrustLock func(context.Context, func() error) error
}

// Client owns one ephemeral exchange and its verified SSH and agent connections.
// Open may be called once; closing its channel or context closes this client.
type Client struct {
	conn        *ssh.Client
	agent       net.Conn
	executable  string
	fingerprint string
	opened      atomic.Bool
	closeOnce   sync.Once
	closeErr    error
}

// Fingerprint returns the verified host key identity observed during Dial.
func (c *Client) Fingerprint() string { return c.fingerprint }

// Close releases the owned connections once and preserves their cleanup result.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = settledConnectionClose(c.conn.Close())
		if c.agent != nil {
			c.closeErr = errors.Join(c.closeErr, settledConnectionClose(c.agent.Close()))
		}
	})
	return c.closeErr
}

// x/crypto may close the socket while processing the peer's final channel close.
// An already-closed owned connection is settled, not a new cleanup failure.
func settledConnectionClose(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

var executablePath = regexp.MustCompile(`^/[A-Za-z0-9._+/-]+$`)

func invocation(path string) (string, error) {
	if path == "" {
		path = "delegate"
	}
	if path != "delegate" && !executablePath.MatchString(path) {
		return "", fmt.Errorf("remote Delegate executable must be a shell-safe absolute path")
	}
	return path + " remote serve --stdio", nil
}

func address(c sshconfig.Connection) string {
	return net.JoinHostPort(c.HostName, strconv.Itoa(c.Port))
}

func verificationAddress(c sshconfig.Connection) string {
	if c.HostKeyAlias != "" {
		return net.JoinHostPort(c.HostKeyAlias, strconv.Itoa(c.Port))
	}
	return address(c)
}

func timeout(value time.Duration) time.Duration {
	if value <= 0 {
		return 10 * time.Second
	}
	return value
}

// Dial verifies host trust and authenticates using keys or the selected local agent.
func Dial(ctx context.Context, connection sshconfig.Connection, options Options) (*Client, error) {
	command, err := invocation(options.DelegateExecutable)
	if err != nil {
		return nil, err
	}
	callback, err := hostCallback(connection, options)
	if err != nil {
		return nil, err
	}
	algorithms, err := knownAlgorithms(connection, options)
	if err != nil {
		return nil, err
	}
	auth, agentConn, err := authentication(ctx, connection, timeout(options.DialTimeout))
	if err != nil {
		return nil, err
	}
	stopAgent := context.AfterFunc(ctx, func() {
		if agentConn != nil {
			closeConnection(agentConn)
		}
	})
	defer stopAgent()
	result := &Client{agent: agentConn, executable: command}
	var fingerprint string
	config := &ssh.ClientConfig{User: connection.User, Auth: auth, HostKeyAlgorithms: algorithms, HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint = hostIdentityFingerprint(key)
		return callback(host, remote, key)
	}}
	dialer := net.Dialer{Timeout: timeout(options.DialTimeout)}
	socket, err := dialer.DialContext(ctx, "tcp", address(connection))
	if err != nil {
		return nil, errors.Join(&Error{Code: "unreachable", cause: err}, closeAgent(agentConn))
	}
	deadline := time.Now().Add(timeout(options.HandshakeTimeout))
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err = socket.SetDeadline(deadline); err != nil {
		return nil, errors.Join(err, socket.Close(), closeAgent(agentConn))
	}
	stop := context.AfterFunc(ctx, func() { closeConnection(socket) })
	native, chans, requests, err := ssh.NewClientConn(socket, address(connection), config)
	stop()
	if err != nil {
		var classified *Error
		if !errors.As(err, &classified) {
			code := "ssh_handshake_failed"
			if strings.Contains(err.Error(), "unable to authenticate") {
				code = "authentication_failed"
			}
			err = &Error{Code: code, cause: err}
		}
		return nil, errors.Join(err, socket.Close(), closeAgent(agentConn))
	}
	if err = socket.SetDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, native.Close(), closeAgent(agentConn))
	}
	result.conn = ssh.NewClient(native, chans, requests)
	result.fingerprint = fingerprint
	return result, nil
}

func closeConnection(c net.Conn) {
	if err := c.Close(); err != nil {
		return
	}
}

func closeAgent(c net.Conn) error {
	if c == nil {
		return nil
	}
	return c.Close()
}

func hostCallback(connection sshconfig.Connection, options Options) (ssh.HostKeyCallback, error) {
	files, filesErr := trustFiles(connection, options)
	if filesErr != nil {
		return nil, filesErr
	}
	verify, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("loading SSH known-hosts: %w", err)
	}
	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint := hostIdentityFingerprint(key)
		if err := verify(verificationAddress(connection), remote, key); err != nil {
			var unknown *knownhosts.KeyError
			var revoked *knownhosts.RevokedError
			code := "host_key_verification_failed"
			switch {
			case errors.As(err, &revoked):
				code = "host_key_revoked"
			case errors.As(err, &unknown):
				code = "host_key_changed"
				if len(unknown.Want) == 0 {
					code = "host_key_unknown"
				}
			}
			return &Error{Code: code, Fingerprint: fingerprint, cause: err}
		}
		if options.PinnedHostKey != "" && options.PinnedHostKey != fingerprint {
			return &Error{Code: "host_key_identity_mismatch", Fingerprint: fingerprint}
		}
		return nil
	}, nil
}

func trustFiles(connection sshconfig.Connection, options Options) ([]string, error) {
	var files []string
	for _, name := range append(append(append([]string(nil), connection.UserKnownHostsFiles...), connection.GlobalKnownHostsFiles...), options.KnownHostsFile) {
		if name == "" {
			continue
		}
		_, err := os.Stat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading SSH known-hosts metadata: %w", err)
		}
		if name == options.KnownHostsFile {
			if err = validateTrustPath(name, false); err != nil {
				return nil, err
			}
		}
		files = append(files, name)
	}
	return files, nil
}

// knownAlgorithms asks the known-hosts verifier for the selected host's accepted
// keys, using an impossible signing identity solely as a lookup probe. It does
// not change verification or trust: the real negotiated key is still checked.
func knownAlgorithms(connection sshconfig.Connection, options Options) ([]string, error) {
	preferred, err := preferredHostAlgorithms(connection, options)
	if err != nil {
		return nil, err
	}
	return append(preferred, ssh.SupportedAlgorithms().HostKeys...), nil
}

// Enrollment preserves configured host/CA preferences. With no matching trust,
// negotiate a bare key so explicit fingerprint enrollment cannot trust a CA.
func enrollmentAlgorithms(connection sshconfig.Connection, options Options) ([]string, error) {
	preferred, err := preferredHostAlgorithms(connection, options)
	if err != nil {
		return nil, err
	}
	if len(preferred) > 0 {
		return append(preferred, ssh.SupportedAlgorithms().HostKeys...), nil
	}
	var plain []string
	for _, algorithm := range ssh.SupportedAlgorithms().HostKeys {
		if !strings.Contains(algorithm, "-cert-") {
			plain = append(plain, algorithm)
		}
	}
	return plain, nil
}

func preferredHostAlgorithms(connection sshconfig.Connection, options Options) ([]string, error) {
	files, err := trustFiles(connection, options)
	if err != nil {
		return nil, err
	}
	verify, err := knownhosts.New(files...)
	if err != nil {
		return nil, err
	}
	probe, err := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	if err != nil {
		return nil, err
	}
	checkErr := verify(verificationAddress(connection), &net.TCPAddr{IP: net.IPv4zero, Port: connection.Port}, probe)
	var wanted *knownhosts.KeyError
	if !errors.As(checkErr, &wanted) {
		return nil, nil
	}
	var preferred []string
	for _, entry := range wanted.Want {
		algorithms, algorithmErr := entryAlgorithms(entry, options.PinnedHostKey)
		if algorithmErr != nil {
			return nil, algorithmErr
		}
		preferred = append(preferred, algorithms...)
	}
	return preferred, nil
}

func entryAlgorithms(entry knownhosts.KnownKey, pin string) ([]string, error) {
	certificate, err := authorityEntry(entry)
	if err != nil {
		return nil, err
	}
	if certificate {
		var algorithms []string
		for _, algorithm := range ssh.SupportedAlgorithms().HostKeys {
			if strings.Contains(algorithm, "-cert-") {
				algorithms = append(algorithms, algorithm)
			}
		}
		return algorithms, nil
	}
	if pin != "" && hostIdentityFingerprint(entry.Key) != pin {
		return nil, nil
	}
	if entry.Key.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}, nil
	}
	return []string{entry.Key.Type()}, nil
}

func authorityEntry(entry knownhosts.KnownKey) (bool, error) {
	file, err := os.Open(entry.Filename)
	if err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(io.LimitReader(file, 4*1024*1024+1))
	reached := false
	var candidate []byte
	for line := 1; scanner.Scan(); line++ {
		if line == entry.Line {
			reached = true
			candidate = append([]byte(nil), scanner.Bytes()...)
			break
		}
	}
	if err = errors.Join(scanner.Err(), file.Close()); err != nil {
		return false, err
	}
	if !reached {
		return false, fmt.Errorf("known-host reference line unavailable within read limit")
	}
	return verifyAuthorityLine(candidate, entry.Key)
}

func verifyAuthorityLine(line []byte, expected ssh.PublicKey) (bool, error) {
	marker, _, key, _, _, err := ssh.ParseKnownHosts(line)
	if err != nil {
		return false, fmt.Errorf("revalidating known-host reference: %w", err)
	}
	if key == nil || expected == nil || string(key.Marshal()) != string(expected.Marshal()) {
		return false, fmt.Errorf("known-host reference changed during algorithm resolution")
	}
	return marker == "cert-authority", nil
}

func authentication(ctx context.Context, c sshconfig.Connection, bound time.Duration) ([]ssh.AuthMethod, net.Conn, error) {
	signers, agentConn, err := agentSigners(ctx, c.IdentityAgent, bound)
	if err != nil {
		return nil, nil, err
	}
	var selected []ssh.Signer
	for _, name := range c.IdentityFiles {
		keys, keyErr := fileSigners(name, signers, c.IdentitiesOnly)
		if keyErr != nil {
			return nil, nil, errors.Join(keyErr, closeAgent(agentConn))
		}
		selected = append(selected, keys...)
	}
	if !c.IdentitiesOnly {
		selected = append(selected, signers...)
	}
	if len(selected) == 0 {
		return nil, nil, errors.Join(&Error{Code: "authentication_unavailable"}, closeAgent(agentConn))
	}
	return []ssh.AuthMethod{ssh.PublicKeys(selected...)}, agentConn, nil
}

func agentSigners(ctx context.Context, path string, bound time.Duration) ([]ssh.Signer, net.Conn, error) {
	if path == "" || path == "none" {
		return nil, nil, nil
	}
	dialer := net.Dialer{Timeout: bound}
	socket, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, nil, &Error{Code: "agent_unavailable", cause: err}
	}
	deadline := time.Now().Add(bound)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err = socket.SetDeadline(deadline); err != nil {
		return nil, nil, errors.Join(err, socket.Close())
	}
	stop := context.AfterFunc(ctx, func() { closeConnection(socket) })
	defer stop()
	signers, err := agent.NewClient(socket).Signers()
	if err != nil {
		return nil, nil, errors.Join(&Error{Code: "agent_unavailable", cause: err}, socket.Close())
	}
	return signers, socket, nil
}

func fileSigners(name string, agents []ssh.Signer, identitiesOnly bool) ([]ssh.Signer, error) {
	content, err := os.ReadFile(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, &Error{Code: "identity_unreadable", cause: err}
	}
	signer, parseErr := ssh.ParsePrivateKey(content)
	if parseErr == nil {
		return []ssh.Signer{signer}, nil
	}
	public, _, _, _, publicErr := ssh.ParseAuthorizedKey(content)
	if publicErr == nil {
		return matchingSigners(public, agents), nil
	}
	var encrypted *ssh.PassphraseMissingError
	if !errors.As(parseErr, &encrypted) {
		return nil, &Error{Code: "identity_invalid", cause: parseErr}
	}
	publicContent, readErr := os.ReadFile(name + ".pub")
	if readErr == nil {
		public, _, _, _, publicErr = ssh.ParseAuthorizedKey(publicContent)
		if publicErr == nil {
			matches := matchingSigners(public, agents)
			if len(matches) > 0 {
				return matches, nil
			}
		}
	}
	if len(agents) > 0 && !identitiesOnly {
		return nil, nil
	}
	return nil, &Error{Code: "auth_key_needs_agent"}
}

func matchingSigners(public ssh.PublicKey, agents []ssh.Signer) []ssh.Signer {
	var selected []ssh.Signer
	for _, candidate := range agents {
		if string(candidate.PublicKey().Marshal()) == string(public.Marshal()) {
			selected = append(selected, candidate)
		}
	}
	return selected
}

// Open starts the fixed Delegate entry point for this client's only exchange.
// Closing its stream or context releases the entire connection without sending
// task cancellation; the remote endpoint owns accepted operation completion.
func (c *Client) Open(ctx context.Context) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.opened.CompareAndSwap(false, true) {
		return nil, errors.New("SSH client exchange already opened")
	}
	// Close caches its result for the exchange cleanup owner to report.
	stop := context.AfterFunc(ctx, func() {
		if closeErr := c.Close(); closeErr != nil {
			return
		}
	})
	session, err := c.conn.NewSession()
	if err != nil {
		stop()
		return nil, errors.Join(err, c.Close())
	}
	reader, err := session.StdoutPipe()
	if err != nil {
		stop()
		return nil, errors.Join(err, c.Close())
	}
	writer, err := session.StdinPipe()
	if err != nil {
		stop()
		return nil, errors.Join(err, c.Close())
	}
	session.Stderr = io.Discard
	if err = session.Start(c.executable); err != nil {
		stop()
		return nil, errors.Join(err, c.Close())
	}
	return &channel{reader: reader, writer: writer, session: session, client: c, ctx: ctx, stop: stop}, nil
}

type channel struct {
	reader    io.Reader
	writer    io.WriteCloser
	session   *ssh.Session
	client    *Client
	ctx       context.Context
	stop      func() bool
	waitOnce  sync.Once
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

func (c *channel) wait() error {
	c.waitOnce.Do(func() { c.waitErr = c.session.Wait() })
	return c.waitErr
}

func (c *channel) terminal(err error) error {
	if err == nil {
		return nil
	}
	waitErr := c.wait()
	if contextErr := c.ctx.Err(); contextErr != nil {
		return contextErr
	}
	if classified := commandExitError(waitErr); classified != nil {
		return classified
	}
	return errors.Join(err, settledWaitError(waitErr))
}

func commandExitError(waitErr error) error {
	var exited *ssh.ExitError
	if errors.As(waitErr, &exited) {
		code := "remote_endpoint_failed"
		switch exited.ExitStatus() {
		case 127:
			code = "remote_delegate_missing"
		case 126:
			code = "remote_delegate_not_executable"
		}
		// A confirmed command exit replaces EOF; preserving EOF would incorrectly
		// classify this endpoint failure as a retryable connection loss.
		return &Error{Code: code, cause: waitErr}
	}
	return nil
}

func (c *channel) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	return n, c.terminal(err)
}

func (c *channel) Write(p []byte) (int, error) {
	n, err := c.writer.Write(p)
	return n, c.terminal(err)
}

// Local connection teardown deliberately prevents further exit-status evidence.
// EOF and ExitMissingError during that settled cleanup are expected; real exit
// statuses and other I/O failures remain visible to the exchange cleanup owner.
func settledWaitError(err error) error {
	var missing *ssh.ExitMissingError
	if errors.Is(err, io.EOF) || errors.As(err, &missing) {
		return nil
	}
	if classified := commandExitError(err); classified != nil {
		return classified
	}
	return err
}

func (c *channel) Close() error {
	c.closeOnce.Do(func() {
		// Close the socket before waiting or sending channel-close: a peer may stop
		// reading SSH packets while keeping its stdout/request channels open.
		closeErr := c.client.Close()
		c.closeErr = errors.Join(closeErr, settledWaitError(c.wait()))
		c.stop()
	})
	return c.closeErr
}

// Enroll adds only the explicitly requested unknown host key to the Delegate-owned
// trust file. Existing changed or revoked keys are never replaced.
func Enroll(ctx context.Context, connection sshconfig.Connection, options Options, expectedFingerprint string) error {
	if options.KnownHostsFile == "" || expectedFingerprint == "" || options.WithTrustLock == nil {
		return fmt.Errorf("enrollment requires a trust file, expected fingerprint and trust owner lock")
	}
	if err := validateTrustPath(options.KnownHostsFile, true); err != nil {
		return err
	}
	verify, err := hostCallback(connection, options)
	if err != nil {
		return err
	}
	algorithms, err := enrollmentAlgorithms(connection, options)
	if err != nil {
		return err
	}
	var accepted ssh.PublicKey
	config := &ssh.ClientConfig{User: connection.User, HostKeyAlgorithms: algorithms, HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
		if verificationErr := verifyEnrollmentHost(host, remote, key, expectedFingerprint, verify); verificationErr != nil {
			return verificationErr
		}
		accepted = key
		return nil
	}}
	dialer := net.Dialer{Timeout: timeout(options.DialTimeout)}
	socket, err := dialer.DialContext(ctx, "tcp", address(connection))
	if err != nil {
		return &Error{Code: "unreachable", cause: err}
	}
	defer func() { closeConnection(socket) }()
	if err = socket.SetDeadline(time.Now().Add(timeout(options.HandshakeTimeout))); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { closeConnection(socket) })
	defer stop()
	native, _, _, handshakeErr := ssh.NewClientConn(socket, address(connection), config)
	if native != nil {
		if err = native.Close(); err != nil {
			return err
		}
	}
	if accepted == nil {
		return &Error{Code: "host_key_enrollment_failed", cause: handshakeErr}
	}
	return options.WithTrustLock(ctx, func() error { return commitTrust(connection, options, accepted) })
}

// Only an exact fingerprint and either existing trust or a genuinely unknown
// bare host key may reach enrollment's accepted-key publication.
func verifyEnrollmentHost(host string, remote net.Addr, key ssh.PublicKey, expected string, verify ssh.HostKeyCallback) error {
	if hostIdentityFingerprint(key) != expected {
		return &Error{Code: "host_key_fingerprint_mismatch"}
	}
	checkErr := verify(host, remote, key)
	if checkErr != nil {
		var knownErr *Error
		if !errors.As(checkErr, &knownErr) || knownErr.Code != "host_key_unknown" {
			return checkErr
		}
	}
	return nil
}

func commitTrust(connection sshconfig.Connection, options Options, key ssh.PublicKey) error {
	if err := validateTrustPath(options.KnownHostsFile, true); err != nil {
		return err
	}
	verify, err := hostCallback(connection, options)
	if err != nil {
		return err
	}
	checkErr := verify(verificationAddress(connection), &net.TCPAddr{IP: net.IPv4zero, Port: connection.Port}, key)
	if checkErr == nil {
		return nil
	}
	var classified *Error
	if !errors.As(checkErr, &classified) || classified.Code != "host_key_unknown" {
		return checkErr
	}
	return appendTrust(options.KnownHostsFile, verificationAddress(connection), key)
}
