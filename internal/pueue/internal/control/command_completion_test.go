package control

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// signaledTestExitError has the process-fact shape of a command terminated by a
// signal without sending one.
type signaledTestExitError struct{}

func (signaledTestExitError) Error() string { return "signal: terminated" }
func (signaledTestExitError) ExitCode() int { return -1 }
func (signaledTestExitError) Exited() bool  { return false }

func naturalTestExit(t *testing.T) error {
	t.Helper()
	err := exec.Command(finiteExecutable(t, "exit 3")).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("fixture did not exit naturally with code 3: %v", err)
	}
	return err
}

type completionTestCase struct {
	name  string
	err   error
	want  commandCompletionKind
	errno syscall.Errno
	kind  string
}

func completionTestCases(t *testing.T) []completionTestCase {
	t.Helper()
	exitErr := naturalTestExit(t)
	readerErr := errors.New("status reader failed")
	pathErrno := &fs.PathError{Op: "open", Path: "/missing", Err: syscall.ENOENT}
	return []completionTestCase{
		{name: "pure exit", err: exitErr, want: commandCompletionExit, kind: "exit"},
		{name: "bounded empty status", err: errors.Join(exitErr, errors.Join(ErrUnknown, io.EOF)), want: commandCompletionExit, kind: "exit"},
		{name: "exit with control limit", err: errors.Join(exitErr, ErrControlLimit), want: commandCompletionExit, kind: "exit"},
		{name: "mixed reader", err: errors.Join(exitErr, readerErr), want: commandCompletionOperational, kind: "wait"},
		{name: "malformed status", err: errors.Join(exitErr, ErrUnknown, fmt.Errorf("%w: invalid scalar", ErrUnknown)), want: commandCompletionOperational, kind: "wait"},
		{name: "unknown without EOF", err: errors.Join(exitErr, fmt.Errorf("%w: missing status field", ErrUnknown)), want: commandCompletionOperational, kind: "wait"},
		{name: "mixed errno", err: errors.Join(exitErr, pathErrno), want: commandCompletionOperational, kind: "wait"},
		{name: "pure errno", err: pathErrno, want: commandCompletionErrno, errno: syscall.ENOENT, kind: "errno"},
		{name: "errno with reader fault", err: errors.Join(pathErrno, readerErr), want: commandCompletionOperational, kind: "wait"},
		{name: "signaled", err: signaledTestExitError{}, want: commandCompletionOperational, kind: "wait"},
		{name: "two exits", err: errors.Join(exitErr, exitErr), want: commandCompletionOperational, kind: "wait"},
		{name: "parsed empty status", err: errors.Join(exitErr, parsedStatusError(t, "")), want: commandCompletionExit, kind: "exit"},
		{name: "nested parsed empty status", err: errors.Join(errors.Join(exitErr), fmt.Errorf("status: %w", parsedStatusError(t, ""))), want: commandCompletionExit, kind: "exit"},
		{name: "parsed whitespace status", err: errors.Join(exitErr, parsedStatusError(t, " ")), want: commandCompletionOperational, kind: "wait"},
		{name: "parsed truncated status", err: errors.Join(exitErr, parsedStatusError(t, `{"tasks"`)), want: commandCompletionOperational, kind: "wait"},
		{name: "wrapped parsed nested truncation", err: fmt.Errorf("status: %w", errors.Join(exitErr, parsedStatusError(t, `{"tasks":{"0":`))), want: commandCompletionOperational, kind: "wait"},
	}
}

// parsedStatusError is the actual parser error for a status stream.
func parsedStatusError(t *testing.T, data string) error {
	t.Helper()
	_, err := protocol.ParseQueueSnapshotStream(strings.NewReader(data), FixtureVersion)
	if err == nil {
		t.Fatalf("status %q parsed successfully", data)
	}
	return err
}

func TestCommandCompletionClassifiesWholeError(t *testing.T) {
	for _, testCase := range completionTestCases(t) {
		if got := classifyCommandCompletion(testCase.err); got.kind != testCase.want || got.errno != testCase.errno {
			t.Errorf("%s: classification = %+v, want kind %d errno %d", testCase.name, got, testCase.want, testCase.errno)
		}
	}
	if got := classifyCommandCompletion(nil); got.kind != commandCompletionOperational {
		t.Errorf("nil error classified as %+v", got)
	}
}

// TestControlOwnerRecordRoundTripKeepsCompletionClass serializes each result
// through the owner's canonical wire record and checks that the reader sees
// the same completion class, so a mixed fault never arrives as a clean exit or
// errno.
func TestControlOwnerRecordRoundTripKeepsCompletionClass(t *testing.T) {
	for _, testCase := range completionTestCases(t) {
		t.Run(testCase.name, func(t *testing.T) {
			sent := CommandResult{Started: true, PID: 42, ExitCode: 3, Err: testCase.err}
			if testCase.want == commandCompletionErrno {
				sent = CommandResult{ExitCode: -1, Err: testCase.err}
			}
			if kind := commandResultRecord(sent).ErrorKind; kind != testCase.kind {
				t.Fatalf("wire kind = %q, want %q", kind, testCase.kind)
			}
			assertReceivedCompletion(t, testCase, roundTripControlOwnerRecord(t, sent).Err)
		})
	}
}

func assertReceivedCompletion(t *testing.T, testCase completionTestCase, received error) {
	t.Helper()
	if got := classifyCommandCompletion(received); got.kind != testCase.want || got.errno != testCase.errno {
		t.Fatalf("reader classification = %+v, want kind %d errno %d: %v", got, testCase.want, testCase.errno, received)
	}
	if errors.Is(received, ErrControlLimit) != errors.Is(testCase.err, ErrControlLimit) {
		t.Fatalf("control limit changed across the wire: %v", received)
	}
	if testCase.errno != 0 && !errors.Is(received, testCase.errno) {
		t.Fatalf("pure errno lost its identity: %v", received)
	}
	if !strings.Contains(received.Error(), testCase.err.Error()) {
		t.Fatalf("diagnostic text was lost: got %q want %q", received.Error(), testCase.err.Error())
	}
}

func roundTripControlOwnerRecord(t *testing.T, sent CommandResult) CommandResult {
	t.Helper()
	data, err := task.MarshalCanonical(commandResultRecord(sent))
	if err != nil {
		t.Fatal(err)
	}
	var record controlOwnerResultRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		t.Fatal(err)
	}
	received, err := commandResultFromRecord(&record)
	if err != nil {
		t.Fatalf("decode owner record: %v", err)
	}
	return received
}

func TestControlOwnerRejectsExitRecordWithoutNaturalCompletion(t *testing.T) {
	for name, record := range map[string]controlOwnerResultRecord{
		"not started":   {ExitCode: -1, ErrorMessage: "exit", ErrorKind: "exit"},
		"no exit code":  {Started: true, PID: 42, ExitCode: -1, ErrorMessage: "exit", ErrorKind: "exit"},
		"zero exitcode": {Started: true, PID: 42, ExitCode: 0, ErrorMessage: "exit", ErrorKind: "exit"},
	} {
		if _, err := commandResultFromRecord(&record); !errors.Is(err, ErrConfiguration) {
			t.Errorf("%s: owner exit record accepted: %v", name, err)
		}
	}
}

// TestControlOwnerProcessClassifiesStatusCompletion runs real status capture
// directly and through the one-shot owner. An empty status stream from a
// naturally failed command crosses as an ordinary completion; a malformed or
// truncated stream crosses as an operational fault.
func TestControlOwnerProcessClassifiesStatusCompletion(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		script string
		id     string
		want   commandCompletionKind
	}{
		{name: "bounded empty status", script: "exit 1", id: "abababababababababababababababab", want: commandCompletionExit},
		{name: "malformed status", script: "printf 'x'; exit 1", id: "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd", want: commandCompletionOperational},
		{name: "truncated status", script: `printf '{"tasks"'; exit 1`, id: "efefefefefefefefefefefefefefefef", want: commandCompletionOperational},
	} {
		executable := finiteExecutable(t, testCase.script)
		for path, start := range map[string]func(*testing.T, *exec.Cmd) *Pending{
			"owner": func(t *testing.T, command *exec.Cmd) *Pending {
				pending := startControlOwnerTest(t, command, testCase.id, FixtureVersion)
				awaitControlOwnerTest(t, pending)
				return pending
			},
			"direct": func(_ *testing.T, command *exec.Cmd) *Pending {
				return startOwnedCaptured(command, "", nil, FixtureVersion)
			},
		} {
			t.Run(testCase.name+"/"+path, func(t *testing.T) {
				command := exec.Command(executable)
				command.Dir = filepath.Dir(executable)
				result, err := observeCommand(t.Context(), start(t, command), 2*time.Second)
				if err == nil || !result.Started || result.ExitCode != 1 {
					t.Fatalf("status result = %+v, %v", result, err)
				}
				if got := classifyCommandCompletion(result.Err); got.kind != testCase.want {
					t.Fatalf("status completion = %+v, want kind %d: %v", got, testCase.want, result.Err)
				}
				if got := privateRefusalCompletion(result, err); got != (testCase.want == commandCompletionExit) {
					t.Fatalf("readiness completion = %t for %v", got, result.Err)
				}
			})
		}
	}
}
