package app

import (
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type nilLifetimeRegistrationReader struct{}

func (nilLifetimeRegistrationReader) ReadLifetimeIntent() (*task.LifetimeIntentRecord, error) {
	return nil, nil
}

func (nilLifetimeRegistrationReader) ReadLifetimeRegistration() (*task.LifetimeRegistrationRecord, error) {
	return nil, nil
}

func TestOpenLifetimeChildRejectsNilRegistration(t *testing.T) {
	parsed := runnerArguments{TaskID: lifetimeGuardianTestTaskID, LifetimeFD: lifetimeChildFD}
	if _, err := openLifetimeChild(parsed, nilLifetimeRegistrationReader{}); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("nil registration error = %v, want evidence fault", err)
	}
}

func TestOpenTaskLifetimeChildRejectsValidTamperedScopeBeforeHello(t *testing.T) {
	store, td, request := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	bootID, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute).UTC()
	planID := "pg:" + strings.Repeat("a", 32)
	scopeID := "pg:" + strings.Repeat("b", 32)
	if _, err = td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: planID,
		BootID:    bootID,
		Deadline:  deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = td.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   scopeID,
		BootID:    bootID,
		Deadline:  deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = td.ReadLifetimeIntent(); err != nil {
		t.Fatalf("valid task intent was rejected: %v", err)
	}
	registration, err := td.ReadLifetimeRegistration()
	if err != nil {
		t.Fatalf("valid tampered task registration was rejected: %v", err)
	}
	if registration.ScopeID != scopeID {
		t.Fatalf("task registration scope id = %q, want %q", registration.ScopeID, scopeID)
	}

	parent, childFile, err := newLifetimeControlPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLifetimeTestConnection(t, parent) })
	t.Cleanup(func() {
		if closeErr := childFile.Close(); closeErr != nil {
			t.Errorf("close child file: %v", closeErr)
		}
	})
	assertLifetimeChildRejectsBeforeHello(t, parent, func() (*openedLifetimeChild, error) {
		return openTaskLifetimeChild(runnerArguments{TaskID: request.TaskID, LifetimeChild: true, LifetimeFD: int(childFile.Fd())}, td)
	}, lifetime.ErrInvalidRef)
}

func TestOpenInspectionLifetimeChildRejectsValidTamperedScopeBeforeHello(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1",
		DefinitionSHA256:   digest,
		HelperExecutable:   "/usr/bin/true",
		HelperSHA256:       digest,
		WorkerExecutable:   "/usr/bin/true",
		WorkerSHA256:       digest,
		Supervisor: task.SupervisorRef{
			ClientExecutable:     "/usr/bin/true",
			ClientSHA256:         digest,
			ResolvedConfigSHA256: digest,
			Endpoint:             "/private/pueue.sock",
			ConfigPath:           filepath.Join(request.CanonicalCwd, "pueue.yml"),
			ConfigDigest:         digest,
			ObservedVersion:      pueue.FixtureVersion,
		},
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	bootID, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	planID := "pg:" + strings.Repeat("a", 32)
	scopeID := "pg:" + strings.Repeat("b", 32)
	if _, err = operation.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: planID,
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = operation.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopeID:   scopeID,
		BootID:    bootID,
		Deadline:  operation.Deadline(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = operation.ReadLifetimeIntent(); err != nil {
		t.Fatalf("valid inspection intent was rejected: %v", err)
	}
	registration, err := operation.ReadLifetimeRegistration()
	if err != nil {
		t.Fatalf("valid tampered inspection registration was rejected: %v", err)
	}
	if registration.ScopeID != scopeID {
		t.Fatalf("inspection registration scope id = %q, want %q", registration.ScopeID, scopeID)
	}

	parent, childFile, err := newLifetimeControlPair()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLifetimeTestConnection(t, parent) })
	t.Cleanup(func() {
		if closeErr := childFile.Close(); closeErr != nil {
			t.Errorf("close child file: %v", closeErr)
		}
	})
	assertLifetimeChildRejectsBeforeHello(t, parent, func() (*openedLifetimeChild, error) {
		return openInspectionLifetimeChild(runnerArguments{TaskID: request.TaskID, LifetimeChild: true, LifetimeFD: int(childFile.Fd())}, operation)
	}, lifetime.ErrInvalidRef)
}

func assertLifetimeChildRejectsBeforeHello(t *testing.T, parent net.Conn, open func() (*openedLifetimeChild, error), want error) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		child, err := open()
		if child != nil {
			err = errors.Join(err, child.Close())
		}
		result <- err
	}()

	if err := parent.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var firstByte [1]byte
	_, readErr := parent.Read(firstByte[:])
	if readErr != nil {
		var networkErr net.Error
		if !errors.As(readErr, &networkErr) || !networkErr.Timeout() {
			t.Fatalf("control read error = %v, want timeout without hello", readErr)
		}
	}
	closeLifetimeTestConnection(t, parent)

	var openErr error
	select {
	case openErr = <-result:
	case <-time.After(time.Second):
		t.Fatal("lifetime child did not reject tampered scope")
	}
	if readErr == nil {
		t.Fatal("lifetime child sent a control hello before rejecting tampered scope")
	}
	if !errors.Is(openErr, want) {
		t.Fatalf("lifetime child error = %v, want %v", openErr, want)
	}
}

func closeLifetimeTestConnection(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Errorf("close parent connection: %v", err)
	}
}
