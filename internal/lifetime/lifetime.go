// Package lifetime contains the narrow operating-system process-scope
// primitive used by a task lifetime owner. It does not decide task policy,
// persist records, or own provider execution.
package lifetime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Kind identifies the operating-system scope backend.
type Kind string

const (
	KindCgroupV2     Kind = "cgroup-v2"
	KindProcessGroup Kind = "process-group"
)

// Assurance describes the containment guarantee provided by a scope.
type Assurance string

const (
	AssuranceExact       Assurance = "exact"
	AssuranceCooperative Assurance = "cooperative"
)

var (
	ErrInvalidRef         = errors.New("lifetime: invalid scope reference")
	ErrInvalidLaunchRef   = errors.New("lifetime: invalid launch reference")
	ErrUnsupported        = errors.New("lifetime: unsupported process scope")
	ErrClosed             = errors.New("lifetime: scope is closed")
	ErrScopeAbsent        = errors.New("lifetime: scope is absent")
	ErrScopeExists        = errors.New("lifetime: scope already exists")
	ErrNilContext         = errors.New("lifetime: nil context")
	ErrNotEmpty           = errors.New("lifetime: scope is not empty")
	ErrConflictingSysProc = errors.New("lifetime: conflicting SysProcAttr")
	ErrUnverifiableLaunch = errors.New("lifetime: launch identity is stale or unverifiable")
)

// Ref is an immutable value identifying one process scope. Its fields are
// private so callers cannot mutate a reference after it has been validated.
type Ref struct {
	kind       Kind
	assurance  Assurance
	identifier string
}

// NewRef constructs a value reference. Platform-specific validation is
// deferred to Open, which also verifies that the referenced OS object still
// has the recorded identity.
func NewRef(kind Kind, assurance Assurance, identifier string) (Ref, error) {
	ref := Ref{kind: kind, assurance: assurance, identifier: identifier}
	if err := validateRefShape(ref); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

func (r Ref) Kind() Kind           { return r.kind }
func (r Ref) Assurance() Assurance { return r.assurance }
func (r Ref) Identifier() string   { return r.identifier }

// LaunchRef is an immutable identity observed immediately after a command
// starts. PGID is zero for cgroup launches and positive for process-group
// launches.
type LaunchRef struct {
	pid        int
	startToken string
	pgid       int
}

// NewLaunchRef is useful when reopening a process-group scope from persisted
// launch evidence. Identity is checked against the live process before any
// destructive operation.
func NewLaunchRef(pid int, startToken string, pgid int) (LaunchRef, error) {
	ref := LaunchRef{pid: pid, startToken: startToken, pgid: pgid}
	if err := validateLaunchRefShape(ref); err != nil {
		return LaunchRef{}, err
	}
	return ref, nil
}

func (r LaunchRef) PID() int           { return r.pid }
func (r LaunchRef) StartToken() string { return r.startToken }
func (r LaunchRef) PGID() int          { return r.pgid }

type platformScope interface {
	configure(*exec.Cmd) error
	observeStarted(int) (LaunchRef, error)
	terminate(context.Context, []LaunchRef) error
	empty([]LaunchRef) (bool, error)
	release() error
	destroyEmpty() error
}

type pendingStartScope interface {
	pendingStartEmpty(context.Context) (bool, error)
}

// Scope owns one OS scope handle and the launch observations made through it.
// The owner must call Configure before Start and ObserveStarted after a
// successful Start. Methods serialize operations on the underlying scope.
type Scope struct {
	mu        sync.Mutex
	ref       Ref
	impl      platformScope
	released  bool
	destroyed bool
}

// BindOwnerProcess lets a cooperative backend bind the calling process as
// its observer-only process-tree owner. Linux process-group scopes use this
// boundary to become a child subreaper before any managed child is started.
// Exact scopes and platforms without an owner primitive need no extra state.
func (s *Scope) BindOwnerProcess() error {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return ErrClosed
	}
	if binder, ok := s.impl.(interface{ bindOwnerProcess() error }); ok {
		return binder.bindOwnerProcess()
	}
	return nil
}

// TrackOwnerProcess records another exact infrastructure process as a
// topology anchor. Anchors are never returned as launch references and are
// therefore never signal targets.
func (s *Scope) TrackOwnerProcess(ref ProcessRef) error {
	if s == nil {
		return ErrClosed
	}
	if err := validateProcessRef(ref); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return ErrClosed
	}
	if tracker, ok := s.impl.(interface{ trackOwnerProcess(ProcessRef) error }); ok {
		return tracker.trackOwnerProcess(ref)
	}
	return nil
}

// Open reopens a previously created scope after validating both its typed
// reference and its platform-specific identity.
func Open(ref Ref) (*Scope, error) {
	if err := validateRefShape(ref); err != nil {
		return nil, err
	}
	return openPlatformScope(ref)
}

// Reference returns the immutable scope reference.
func (s *Scope) Reference() Ref {
	if s == nil {
		return Ref{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if provider, ok := s.impl.(interface{ reference() Ref }); ok {
		return provider.reference()
	}
	return s.ref
}

// Configure installs the backend's launch containment into cmd. It must be
// called before cmd.Start.
func (s *Scope) Configure(cmd *exec.Cmd) error {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return ErrClosed
	}
	return s.impl.configure(cmd)
}

// ObserveStarted records a stable process identity and verifies scope
// membership where the platform exposes that observation.
func (s *Scope) ObserveStarted(pid int) (LaunchRef, error) {
	if s == nil {
		return LaunchRef{}, ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return LaunchRef{}, ErrClosed
	}
	return s.impl.observeStarted(pid)
}

// Terminate stops only the exact scope and waits for independent emptiness.
// Repeating it after the scope is already empty is safe.
func (s *Scope) Terminate(ctx context.Context, launchRefs []LaunchRef) error {
	if s == nil {
		return ErrClosed
	}
	if ctx == nil {
		return ErrNilContext
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return ErrClosed
	}
	return s.impl.terminate(ctx, cloneLaunchRefs(launchRefs))
}

// Empty reports only an independently observed empty scope. Observation
// errors are returned to the caller and are never converted to true.
func (s *Scope) Empty(launchRefs []LaunchRef) (bool, error) {
	return s.EmptyContext(context.Background(), launchRefs)
}

// EmptyContext is Empty with a caller-owned observation lifetime. Backends
// that perform a process census stop that work when ctx expires.
func (s *Scope) EmptyContext(ctx context.Context, launchRefs []LaunchRef) (bool, error) {
	if s == nil {
		return false, ErrClosed
	}
	if ctx == nil {
		return false, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return false, ErrClosed
	}
	if contextual, ok := s.impl.(interface {
		emptyContext(context.Context, []LaunchRef) (bool, error)
	}); ok {
		return contextual.emptyContext(ctx, cloneLaunchRefs(launchRefs))
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.impl.empty(cloneLaunchRefs(launchRefs))
}

// PendingStartEmptyContext proves that a worker which has not durably
// acknowledged a native start left no process in its scope. Exact backends
// use their native membership primitive. Cooperative backends must implement
// an explicit pending-start census; they cannot treat an empty launch list as
// proof because the launch may have happened before acknowledgement.
func (s *Scope) PendingStartEmptyContext(ctx context.Context) (bool, error) {
	if s == nil {
		return false, ErrClosed
	}
	if ctx == nil {
		return false, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return false, ErrClosed
	}
	if pending, ok := s.impl.(pendingStartScope); ok {
		return pending.pendingStartEmpty(ctx)
	}
	if s.ref.assurance != AssuranceExact {
		return false, fmt.Errorf("%w: cooperative scope has no pending-start proof", ErrUnverifiableLaunch)
	}
	return s.impl.empty(nil)
}

// Release closes this scope's local handles without disposing its native
// resource. It is safe to call repeatedly. A failed release remains
// retryable and leaves the scope unreleased.
func (s *Scope) Release() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return nil
	}
	if err := s.impl.release(); err != nil {
		return err
	}
	s.released = true
	return nil
}

// DestroyEmpty disposes the native scope after its implementation has
// independently verified that it is empty. It is safe to call repeatedly
// after successful disposal. It does not release local handles; the owner must
// call Release separately. This ordering keeps a handle-close failure from
// causing a removed resource to be disposed again. Release intentionally makes
// later disposal unavailable.
func (s *Scope) DestroyEmpty() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return nil
	}
	if s.released {
		return ErrClosed
	}
	if !s.destroyed {
		if err := s.impl.destroyEmpty(); err != nil {
			return err
		}
		s.destroyed = true
	}
	return nil
}

func newScope(ref Ref, impl platformScope) *Scope {
	return &Scope{ref: ref, impl: impl}
}

func validateCreateIDs(rootID, taskID string) error {
	for name, value := range map[string]string{"root ID": rootID, "task ID": taskID} {
		if value == "" || strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%w: %s is empty or contains NUL", ErrInvalidRef, name)
		}
	}
	return nil
}

func validateRefShape(ref Ref) error {
	switch ref.kind {
	case KindCgroupV2:
		if ref.assurance != AssuranceExact {
			return fmt.Errorf("%w: cgroup-v2 requires exact assurance", ErrInvalidRef)
		}
	case KindProcessGroup:
		if ref.assurance != AssuranceCooperative {
			return fmt.Errorf("%w: process-group requires cooperative assurance", ErrInvalidRef)
		}
	default:
		return fmt.Errorf("%w: unknown scope kind %q", ErrInvalidRef, ref.kind)
	}
	if ref.identifier == "" || strings.ContainsRune(ref.identifier, '\x00') {
		return fmt.Errorf("%w: empty or NUL scope identifier", ErrInvalidRef)
	}
	return nil
}

func validateLaunchRefShape(ref LaunchRef) error {
	if ref.pid <= 0 || ref.startToken == "" || strings.ContainsRune(ref.startToken, '\x00') || ref.pgid < 0 {
		return ErrInvalidLaunchRef
	}
	return nil
}

func cloneLaunchRefs(refs []LaunchRef) []LaunchRef {
	return append([]LaunchRef(nil), refs...)
}

const (
	processGroupTerminationLimit = 5 * time.Second
	processGroupTermGrace        = 250 * time.Millisecond
	processScopePollInterval     = 10 * time.Millisecond
)

type sleepFunc func(context.Context, time.Duration) error

func waitForEmpty(ctx context.Context, pollInterval time.Duration, empty func() (bool, error), sleep sleepFunc) error {
	if ctx == nil {
		return ErrNilContext
	}
	if pollInterval <= 0 || empty == nil || sleep == nil {
		return fmt.Errorf("%w: invalid empty polling arguments", ErrInvalidRef)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		isEmpty, err := empty()
		if err != nil {
			return err
		}
		if isEmpty {
			return nil
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return err
		}
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
