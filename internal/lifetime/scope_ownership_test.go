package lifetime

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

type ownershipTestScope struct {
	releaseCalls  int
	destroyCalls  int
	releaseErrors []error
	destroyErrors []error
}

func (s *ownershipTestScope) configure(*exec.Cmd) error {
	return nil
}

func (s *ownershipTestScope) observeStarted(int) (LaunchRef, error) {
	return LaunchRef{}, nil
}

func (s *ownershipTestScope) terminate(context.Context, []LaunchRef) error {
	return nil
}

func (s *ownershipTestScope) empty([]LaunchRef) (bool, error) {
	return true, nil
}

func (s *ownershipTestScope) release() error {
	s.releaseCalls++
	return takeOwnershipTestError(&s.releaseErrors)
}

func (s *ownershipTestScope) destroyEmpty() error {
	s.destroyCalls++
	return takeOwnershipTestError(&s.destroyErrors)
}

func takeOwnershipTestError(errors *[]error) error {
	if len(*errors) == 0 {
		return nil
	}
	err := (*errors)[0]
	*errors = (*errors)[1:]
	return err
}

func newOwnershipTestScope(impl platformScope) *Scope {
	ref, err := NewRef(KindProcessGroup, AssuranceCooperative, "pg:"+strings.Repeat("a", 32))
	if err != nil {
		panic(err)
	}
	return newScope(ref, impl)
}

func TestScopeReleaseDoesNotDestroyNativeResource(t *testing.T) {
	impl := &ownershipTestScope{}
	scope := newOwnershipTestScope(impl)

	if err := scope.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := scope.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if impl.releaseCalls != 1 {
		t.Fatalf("release calls = %d, want 1", impl.releaseCalls)
	}
	if impl.destroyCalls != 0 {
		t.Fatalf("destroy calls = %d, want 0", impl.destroyCalls)
	}
	if err := scope.DestroyEmpty(); !errors.Is(err, ErrClosed) {
		t.Fatalf("DestroyEmpty after Release = %v, want ErrClosed", err)
	}
	if impl.destroyCalls != 0 {
		t.Fatalf("destroy calls after rejected disposal = %d, want 0", impl.destroyCalls)
	}
}

func TestScopeReleaseFailureRemainsRetryable(t *testing.T) {
	wantErr := errors.New("release failed")
	impl := &ownershipTestScope{releaseErrors: []error{wantErr, nil}}
	scope := newOwnershipTestScope(impl)

	if err := scope.Release(); !errors.Is(err, wantErr) {
		t.Fatalf("first Release = %v, want %v", err, wantErr)
	}
	if err := scope.Release(); err != nil {
		t.Fatalf("retry Release: %v", err)
	}
	if err := scope.Release(); err != nil {
		t.Fatalf("repeated Release: %v", err)
	}
	if impl.releaseCalls != 2 {
		t.Fatalf("release calls = %d, want 2", impl.releaseCalls)
	}
}

func TestScopeDestroyEmptyRefusesAndRetriesPopulatedResource(t *testing.T) {
	impl := &ownershipTestScope{destroyErrors: []error{ErrNotEmpty, nil}}
	scope := newOwnershipTestScope(impl)

	if err := scope.DestroyEmpty(); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("first DestroyEmpty = %v, want ErrNotEmpty", err)
	}
	if empty, err := scope.Empty(nil); err != nil || !empty {
		t.Fatalf("scope after refused DestroyEmpty = (%t, %v), want true/nil", empty, err)
	}
	if err := scope.DestroyEmpty(); err != nil {
		t.Fatalf("retry DestroyEmpty: %v", err)
	}
	if err := scope.DestroyEmpty(); err != nil {
		t.Fatalf("repeated DestroyEmpty: %v", err)
	}
	if impl.destroyCalls != 2 {
		t.Fatalf("destroy calls = %d, want 2", impl.destroyCalls)
	}
	if impl.releaseCalls != 0 {
		t.Fatalf("release calls = %d, want 0", impl.releaseCalls)
	}
	if err := scope.Release(); err != nil {
		t.Fatalf("Release after DestroyEmpty: %v", err)
	}
}
