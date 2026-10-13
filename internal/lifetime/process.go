package lifetime

import (
	"context"
	"fmt"
	"strings"
)

// ProcessRef is the immutable identity of the lifetime worker. It fences
// recovery from publishing scope termination while a worker that received a
// launch acknowledgement can still start native work.
type ProcessRef struct {
	pid        int
	startToken string
}

func NewProcessRef(pid int, startToken string) (ProcessRef, error) {
	ref := ProcessRef{pid: pid, startToken: startToken}
	if err := validateProcessRef(ref); err != nil {
		return ProcessRef{}, err
	}
	return ref, nil
}

func (r ProcessRef) PID() int           { return r.pid }
func (r ProcessRef) StartToken() string { return r.startToken }

// ObserveProcess records the platform start identity for one live process.
func ObserveProcess(pid int) (ProcessRef, error) {
	present, token, err := observeProcessIdentity(pid)
	if err != nil {
		return ProcessRef{}, err
	}
	if !present {
		return ProcessRef{}, fmt.Errorf("%w: process %d is absent", ErrUnverifiableLaunch, pid)
	}
	return NewProcessRef(pid, token)
}

// ObserveStartedProcess records a child identity before its single Wait. An
// unreaped zombie remains identity-stable because its PID cannot yet be reused.
func ObserveStartedProcess(pid int) (ProcessRef, error) {
	present, token, err := observeStartedProcessIdentity(pid)
	if err != nil {
		return ProcessRef{}, err
	}
	if !present {
		return ProcessRef{}, fmt.Errorf("%w: process %d is absent", ErrUnverifiableLaunch, pid)
	}
	return NewProcessRef(pid, token)
}

// ProcessPresent reports whether the exact recorded process remains live. A
// reused PID is not the recorded worker and therefore reports false.
func ProcessPresent(ref ProcessRef) (bool, error) {
	if err := validateProcessRef(ref); err != nil {
		return false, err
	}
	present, token, err := observeProcessIdentity(ref.pid)
	if err != nil || !present {
		return present, err
	}
	return token == ref.startToken, nil
}

// WaitProcessAbsent waits until the exact worker exits. It never signals the
// process and treats PID reuse as proof that the recorded worker is gone.
func WaitProcessAbsent(ctx context.Context, ref ProcessRef) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := validateProcessRef(ref); err != nil {
		return err
	}
	return waitForEmpty(ctx, processScopePollInterval, func() (bool, error) {
		present, err := ProcessPresent(ref)
		return !present, err
	}, sleepContext)
}

func validateProcessRef(ref ProcessRef) error {
	if ref.pid <= 0 || ref.startToken == "" || strings.ContainsRune(ref.startToken, '\x00') {
		return ErrInvalidLaunchRef
	}
	return nil
}
