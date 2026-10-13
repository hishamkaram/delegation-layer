//go:build darwin

package lifetime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	processGroupPinTokenEnv   = "DELEGATION_LAYER_PROCESS_GROUP_PIN_TOKEN"
	processGroupPinListenerFD = 3
	processGroupPinReadyFD    = 4
	processGroupPinVersion    = 1
	processGroupPinRefVersion = "darwin-pin-v1"
	processGroupPinTimeout    = 5 * time.Second
	processGroupPinMaxFrame   = 512
)

const (
	processGroupPinProbe    = "probe"
	processGroupPinTerm     = "term"
	processGroupPinKill     = "kill"
	processGroupPinShutdown = "shutdown"
)

type darwinProcessGroupPin struct {
	ref       ProcessRef
	address   string
	token     string
	cmd       *exec.Cmd
	owned     bool
	destroyed bool
	waitOnce  sync.Once
	waitDone  chan error
}

type processGroupPinRequest struct {
	Version   int    `json:"version"`
	Token     string `json:"token"`
	Operation string `json:"operation"`
	PID       int    `json:"pid"`
	PGID      int    `json:"pgid"`
}

type processGroupPinResponse struct {
	Version int  `json:"version"`
	OK      bool `json:"ok"`
}

func (p *darwinProcessGroupPin) processRef() ProcessRef { return p.ref }

func validateProcessGroupReferenceIdentifier(identifier string) error {
	if validateProcessGroupPlanIdentifier(identifier) == nil {
		return nil
	}
	_, _, err := decodeDarwinProcessGroupPinReference(identifier)
	return err
}

func openProcessGroupPlatformState(ref Ref) (string, processGroupPin, error) {
	if validateProcessGroupPlanIdentifier(ref.identifier) == nil {
		if err := darwinProcessInfoCapability(); err != nil {
			return "", nil, err
		}
		return ref.identifier, nil, nil
	}
	identifier, pin, err := decodeDarwinProcessGroupPinReference(ref.identifier)
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), processGroupPinTimeout)
	defer cancel()
	if err = pin.probe(ctx); err != nil {
		if errors.Is(err, ErrScopeAbsent) {
			return "", nil, ErrScopeAbsent
		}
		return "", nil, err
	}
	return identifier, pin, nil
}

func bindProcessGroupPlatformState(scope *processGroupScope) error {
	if scope == nil {
		return ErrClosed
	}
	if scope.pin == nil {
		return darwinProcessInfoCapability()
	}
	pin, ok := scope.pin.(*darwinProcessGroupPin)
	if !ok {
		return ErrInvalidRef
	}
	ctx, cancel := context.WithTimeout(context.Background(), processGroupPinTimeout)
	defer cancel()
	return pin.probe(ctx)
}

func processGroupPlatformReference(scope *processGroupScope) Ref {
	if scope == nil {
		return Ref{}
	}
	identifier := scope.identifier
	if pin, ok := scope.pin.(*darwinProcessGroupPin); ok {
		identifier = encodeDarwinProcessGroupPinReference(scope.identifier, pin)
	}
	return Ref{kind: KindProcessGroup, assurance: AssuranceCooperative, identifier: identifier}
}

func validateProcessGroupLaunch(scope *processGroupScope, pid, pgid int) error {
	if scope == nil {
		return fmt.Errorf("%w: Darwin process-group scope is unavailable", ErrUnverifiableLaunch)
	}
	if scope.pin == nil {
		if pgid != pid {
			return fmt.Errorf("%w: PID %d is in PGID %d, want its own group", ErrUnverifiableLaunch, pid, pgid)
		}
		return nil
	}
	pinPID := scope.pin.processRef().PID()
	if pgid != pinPID {
		return fmt.Errorf("%w: PID %d is in unpinned PGID %d, want %d", ErrUnverifiableLaunch, pid, pgid, pinPID)
	}
	return nil
}

func validateProcessGroupSignalTargets(scope *processGroupScope, groups map[int][]LaunchRef) error {
	if len(groups) == 0 {
		return nil
	}
	if scope == nil {
		return fmt.Errorf("%w: Darwin process-group scope is unavailable", ErrUnverifiableLaunch)
	}
	if scope.pin == nil {
		for pgid := range groups {
			if err := validateSignalTarget(pgid); err != nil {
				return err
			}
		}
		return nil
	}
	pinPID := scope.pin.processRef().PID()
	for pgid := range groups {
		if pgid != pinPID {
			return fmt.Errorf("%w: process group %d has no identity-bound Darwin signal authority", ErrUnverifiableLaunch, pgid)
		}
	}
	return nil
}

func processGroupPlatformInfrastructure(scope *processGroupScope) []ProcessRef {
	if scope == nil || scope.pin == nil {
		return nil
	}
	return []ProcessRef{scope.pin.processRef()}
}

func signalDarwinPinnedProcessGroup(ctx context.Context, scope *processGroupScope, pgid int, signal syscall.Signal) error {
	if scope == nil || scope.pin == nil {
		return fmt.Errorf("%w: Darwin process-group pin is unavailable", ErrUnverifiableLaunch)
	}
	pin, ok := scope.pin.(*darwinProcessGroupPin)
	if !ok || pin.ref.PID() != pgid {
		return fmt.Errorf("%w: process group %d is not pinned", ErrUnverifiableLaunch, pgid)
	}
	operation := ""
	switch signal {
	case syscall.SIGTERM:
		operation = processGroupPinTerm
	case syscall.SIGKILL:
		operation = processGroupPinKill
	default:
		return fmt.Errorf("%w: unsupported Darwin process-group signal %d", ErrInvalidRef, signal)
	}
	return pin.request(ctx, operation)
}

func releaseProcessGroupPlatformState(scope *processGroupScope) error {
	if scope == nil || scope.pin == nil {
		return nil
	}
	pin, ok := scope.pin.(*darwinProcessGroupPin)
	if !ok {
		return ErrInvalidRef
	}
	if pin.owned && !pin.destroyed {
		return ErrNotEmpty
	}
	return nil
}

func destroyProcessGroupPlatformState(scope *processGroupScope) error {
	if scope == nil || scope.pin == nil {
		return nil
	}
	pin, ok := scope.pin.(*darwinProcessGroupPin)
	if !ok {
		return ErrInvalidRef
	}
	if pin.destroyed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), processGroupPinTimeout)
	defer cancel()
	empty, err := scope.observeEmptyContext(ctx, scope.mergeLaunchRefs(nil), false)
	if err != nil {
		return err
	}
	if !empty {
		return ErrNotEmpty
	}
	state, err := pin.observe()
	if err != nil {
		return err
	}
	if state.present && !state.zombie {
		if err = pin.request(ctx, processGroupPinShutdown); err != nil && !errors.Is(err, ErrScopeAbsent) {
			return err
		}
	}
	if pin.owned {
		if err = pin.wait(ctx); err != nil {
			return err
		}
	} else if err = waitDarwinLegacyProcessAbsent(ctx, pin.ref); err != nil {
		return err
	}
	pin.destroyed = true
	return nil
}

func waitDarwinLegacyProcessAbsent(ctx context.Context, ref ProcessRef) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := validateProcessRef(ref); err != nil {
		return err
	}
	return waitForEmpty(ctx, processScopePollInterval, func() (bool, error) {
		present, _, token, err := inspectDarwinProcessIdentity(ref.PID())
		if err != nil {
			return false, err
		}
		return !present || !processIdentityTokensMatch(token, ref.StartToken()), nil
	}, sleepContext)
}

type darwinProcessGroupPinState struct {
	present bool
	zombie  bool
}

func (p *darwinProcessGroupPin) observe() (darwinProcessGroupPinState, error) {
	present, zombie, token, err := inspectDarwinProcessIdentity(p.ref.PID())
	if err != nil {
		return darwinProcessGroupPinState{}, err
	}
	if !present {
		return darwinProcessGroupPinState{}, nil
	}
	if !processIdentityTokensMatch(token, p.ref.StartToken()) {
		return darwinProcessGroupPinState{}, fmt.Errorf("%w: Darwin process-group pin identity changed", ErrUnverifiableLaunch)
	}
	pgid, err := processGroupID(p.ref.PID())
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return darwinProcessGroupPinState{}, nil
		}
		return darwinProcessGroupPinState{}, err
	}
	if pgid != p.ref.PID() {
		return darwinProcessGroupPinState{}, fmt.Errorf("%w: Darwin process-group pin left its group", ErrUnverifiableLaunch)
	}
	return darwinProcessGroupPinState{present: true, zombie: zombie}, nil
}

func (p *darwinProcessGroupPin) probe(ctx context.Context) error {
	state, err := p.observe()
	if err != nil {
		return err
	}
	if !state.present {
		return ErrScopeAbsent
	}
	if state.zombie {
		return fmt.Errorf("%w: Darwin process-group pin is a zombie", ErrUnverifiableLaunch)
	}
	return p.requestUnchecked(ctx, processGroupPinProbe)
}

func (p *darwinProcessGroupPin) request(ctx context.Context, operation string) error {
	state, err := p.observe()
	if err != nil {
		return err
	}
	if !state.present {
		return ErrScopeAbsent
	}
	if state.zombie {
		return fmt.Errorf("%w: Darwin process-group pin is not serving", ErrUnverifiableLaunch)
	}
	return p.requestUnchecked(ctx, operation)
}

func (p *darwinProcessGroupPin) requestUnchecked(ctx context.Context, operation string) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "tcp4", p.address)
	if err != nil {
		return fmt.Errorf("%w: contact Darwin process-group pin: %w", ErrUnverifiableLaunch, err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err = connection.SetDeadline(deadline); err != nil {
			return err
		}
	}
	request := processGroupPinRequest{
		Version: processGroupPinVersion, Token: p.token, Operation: operation,
		PID: p.ref.PID(), PGID: p.ref.PID(),
	}
	if err = json.NewEncoder(connection).Encode(request); err != nil {
		return err
	}
	var response processGroupPinResponse
	if err = decodeProcessGroupPinFrame(connection, &response); err != nil {
		return err
	}
	if response.Version != processGroupPinVersion || !response.OK {
		return fmt.Errorf("%w: Darwin process-group pin refused request", ErrUnverifiableLaunch)
	}
	return nil
}

func (p *darwinProcessGroupPin) wait(ctx context.Context) error {
	if p.cmd == nil {
		return ErrInvalidRef
	}
	p.waitOnce.Do(func() {
		p.waitDone = make(chan error, 1)
		go func() { p.waitDone <- p.cmd.Wait() }()
	})
	select {
	case err := <-p.waitDone:
		var exitErr *exec.ExitError
		if err == nil || errors.As(err, &exitErr) {
			return nil
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func startDarwinProcessGroupPin() (*darwinProcessGroupPin, error) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("create Darwin process-group pin listener: %w", err)
	}
	listenerFile, err := listener.File()
	if err != nil {
		return nil, errors.Join(err, listener.Close())
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		return nil, errors.Join(err, listenerFile.Close())
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, listenerFile.Close())
	}
	token, err := newProcessGroupPinToken()
	if err != nil {
		return nil, errors.Join(err, listenerFile.Close(), readyReader.Close(), readyWriter.Close())
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.Join(err, listenerFile.Close(), readyReader.Close(), readyWriter.Close())
	}
	cmd := exec.Command(executable)
	cmd.Env = appendProcessGroupPinEnvironment(os.Environ(), token)
	cmd.ExtraFiles = []*os.File{listenerFile, readyWriter}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		return nil, errors.Join(err, listenerFile.Close(), readyReader.Close(), readyWriter.Close())
	}
	closeErr := errors.Join(listenerFile.Close(), readyWriter.Close())
	if closeErr != nil {
		return nil, errors.Join(closeErr, abortDarwinProcessGroupPin(cmd), readyReader.Close())
	}
	if err = waitDarwinProcessGroupPinReady(readyReader); err != nil {
		return nil, errors.Join(err, abortDarwinProcessGroupPin(cmd))
	}
	present, startToken, err := observeStartedProcessIdentity(cmd.Process.Pid)
	if err != nil || !present {
		return nil, errors.Join(err, ErrUnverifiableLaunch, abortDarwinProcessGroupPin(cmd))
	}
	pgid, err := processGroupID(cmd.Process.Pid)
	if err != nil || pgid != cmd.Process.Pid {
		return nil, errors.Join(err, ErrUnverifiableLaunch, abortDarwinProcessGroupPin(cmd))
	}
	ref, err := NewProcessRef(cmd.Process.Pid, startToken)
	if err != nil {
		return nil, errors.Join(err, abortDarwinProcessGroupPin(cmd))
	}
	pin := &darwinProcessGroupPin{ref: ref, address: address, token: token, cmd: cmd, owned: true}
	ctx, cancel := context.WithTimeout(context.Background(), processGroupPinTimeout)
	defer cancel()
	if err = pin.probe(ctx); err != nil {
		return nil, errors.Join(err, abortDarwinProcessGroupPin(cmd))
	}
	return pin, nil
}

func waitDarwinProcessGroupPinReady(reader *os.File) error {
	defer reader.Close()
	result := make(chan error, 1)
	go func() {
		var ready [1]byte
		_, err := io.ReadFull(reader, ready[:])
		if err == nil && ready[0] != 1 {
			err = ErrUnverifiableLaunch
		}
		result <- err
	}()
	timer := time.NewTimer(processGroupPinTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

func abortDarwinProcessGroupPin(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	killErr := cmd.Process.Kill()
	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		waitErr = nil
	}
	return errors.Join(killErr, waitErr)
}

func newProcessGroupPinToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func appendProcessGroupPinEnvironment(environment []string, token string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.HasPrefix(entry, processGroupPinTokenEnv+"=") || strings.HasPrefix(entry, processScopeMarkerEnvironment+"=") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, processGroupPinTokenEnv+"="+token)
}

func encodeDarwinProcessGroupPinReference(identifier string, pin *darwinProcessGroupPin) string {
	return strings.Join([]string{
		identifier,
		processGroupPinRefVersion,
		strconv.Itoa(pin.ref.PID()),
		base64.RawURLEncoding.EncodeToString([]byte(pin.ref.StartToken())),
		base64.RawURLEncoding.EncodeToString([]byte(pin.address)),
		pin.token,
	}, "|")
}

func decodeDarwinProcessGroupPinReference(identifier string) (string, *darwinProcessGroupPin, error) {
	parts := strings.Split(identifier, "|")
	if len(parts) != 6 || parts[1] != processGroupPinRefVersion || validateProcessGroupPlanIdentifier(parts[0]) != nil {
		return "", nil, fmt.Errorf("%w: Darwin process-group pin reference is malformed", ErrInvalidRef)
	}
	pid, err := strconv.Atoi(parts[2])
	if err != nil || pid <= 1 || strconv.Itoa(pid) != parts[2] {
		return "", nil, fmt.Errorf("%w: Darwin process-group pin PID is malformed", ErrInvalidRef)
	}
	startToken, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(startToken) == 0 || strings.ContainsRune(string(startToken), '\x00') {
		return "", nil, fmt.Errorf("%w: Darwin process-group pin identity is malformed", ErrInvalidRef)
	}
	addressBytes, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return "", nil, fmt.Errorf("%w: Darwin process-group pin address is malformed", ErrInvalidRef)
	}
	address := string(addressBytes)
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil || !endpoint.Addr().Is4() || !endpoint.Addr().IsLoopback() || endpoint.Port() == 0 || endpoint.String() != address {
		return "", nil, fmt.Errorf("%w: Darwin process-group pin address is malformed", ErrInvalidRef)
	}
	tokenBytes, err := hex.DecodeString(parts[5])
	if err != nil || len(tokenBytes) != 32 || hex.EncodeToString(tokenBytes) != parts[5] {
		return "", nil, fmt.Errorf("%w: Darwin process-group pin token is malformed", ErrInvalidRef)
	}
	ref, err := NewProcessRef(pid, string(startToken))
	if err != nil {
		return "", nil, err
	}
	return parts[0], &darwinProcessGroupPin{ref: ref, address: address, token: parts[5]}, nil
}

// RunProcessGroupPinMode handles the private Darwin group authority process.
// It is called before public argument parsing by delegate-run.
func RunProcessGroupPinMode(_ []string) (int, bool) {
	if os.Getenv(processGroupPinTokenEnv) == "" {
		return 0, false
	}
	if runDarwinProcessGroupPin() != nil {
		return 1, true
	}
	return 0, true
}

func runDarwinProcessGroupPin() error {
	token := os.Getenv(processGroupPinTokenEnv)
	decodedToken, err := hex.DecodeString(token)
	if err != nil || len(decodedToken) != 32 || hex.EncodeToString(decodedToken) != token {
		return ErrInvalidRef
	}
	pid := os.Getpid()
	pgid, err := processGroupID(pid)
	if err != nil || pgid != pid {
		return errors.Join(err, ErrUnverifiableLaunch)
	}
	signal.Ignore(syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	listenerFile := os.NewFile(processGroupPinListenerFD, "delegate-process-group-pin-listener")
	readyFile := os.NewFile(processGroupPinReadyFD, "delegate-process-group-pin-ready")
	if listenerFile == nil || readyFile == nil {
		return ErrInvalidRef
	}
	listener, err := net.FileListener(listenerFile)
	closeErr := listenerFile.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr, readyFile.Close())
	}
	if _, err = readyFile.Write([]byte{1}); err != nil {
		return errors.Join(err, readyFile.Close(), listener.Close())
	}
	if err = readyFile.Close(); err != nil {
		return errors.Join(err, listener.Close())
	}
	defer listener.Close()
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return acceptErr
		}
		stop, serveErr := serveDarwinProcessGroupPin(connection, token, pid)
		if serveErr != nil {
			continue
		}
		if stop {
			return nil
		}
	}
}

func serveDarwinProcessGroupPin(connection net.Conn, token string, pid int) (stop bool, resultErr error) {
	closed := false
	closeConnection := func() error {
		if closed {
			return nil
		}
		closed = true
		return connection.Close()
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeConnection())
	}()
	if err := connection.SetDeadline(time.Now().Add(processGroupPinTimeout)); err != nil {
		return false, err
	}
	var request processGroupPinRequest
	if err := decodeProcessGroupPinFrame(connection, &request); err != nil {
		return false, err
	}
	if request.Version != processGroupPinVersion || request.Token != token || request.PID != pid || request.PGID != pid {
		return false, ErrInvalidRef
	}
	response := processGroupPinResponse{Version: processGroupPinVersion, OK: true}
	switch request.Operation {
	case processGroupPinProbe:
		return false, json.NewEncoder(connection).Encode(response)
	case processGroupPinTerm:
		if err := unix.Kill(0, unix.SIGTERM); err != nil {
			return false, err
		}
		return false, json.NewEncoder(connection).Encode(response)
	case processGroupPinKill:
		if err := json.NewEncoder(connection).Encode(response); err != nil {
			return false, err
		}
		if err := closeConnection(); err != nil {
			return false, err
		}
		return false, unix.Kill(0, unix.SIGKILL)
	case processGroupPinShutdown:
		if err := json.NewEncoder(connection).Encode(response); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, ErrInvalidRef
	}
}

func decodeProcessGroupPinFrame[T any](reader io.Reader, value *T) error {
	limited := io.LimitReader(reader, processGroupPinMaxFrame+1)
	buffered := bufio.NewReaderSize(limited, processGroupPinMaxFrame+1)
	data, err := buffered.ReadBytes('\n')
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > processGroupPinMaxFrame {
		return ErrInvalidRef
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ErrInvalidRef
	}
	return nil
}
