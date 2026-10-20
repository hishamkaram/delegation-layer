package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var errTestJoinedStreamClose = errors.New("close command stdout stream")

type requireDetachedCase struct {
	name      string
	stdout    string
	stderr    string
	runErr    error
	wantErr   []error
	wantClean bool
}

// requireDetached accepts only one clean native not-symbolic exit; a joined
// execution fault or an unexpected diagnostic never reads as detached.
func TestRequireDetachedAcceptsOnlyCleanNotSymbolicExit(t *testing.T) {
	tests := []requireDetachedCase{
		{name: "clean exit 1", runErr: gitTestExitError(1), wantClean: true},
		{name: "attached", stdout: "refs/heads/main\n", wantErr: []error{ErrIdentityMismatch}},
		{
			name:    "exit 1 with joined close fault",
			runErr:  errors.Join(gitTestExitError(1), errTestJoinedStreamClose),
			wantErr: []error{errTestJoinedStreamClose},
		},
		{
			name:    "exit 1 with context deadline",
			runErr:  errors.Join(gitTestExitError(1), context.DeadlineExceeded),
			wantErr: []error{context.DeadlineExceeded},
		},
		{name: "exit 1 with stderr", runErr: gitTestExitError(1), stderr: "warning: unexpected\n"},
		{name: "signal exit", runErr: gitTestExitError(-1), wantErr: []error{gitTestExitError(-1)}},
		{name: "fatal exit", runErr: gitTestExitError(128), wantErr: []error{gitTestExitError(128)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { runRequireDetachedCase(t, test) })
	}
}

func runRequireDetachedCase(t *testing.T, test requireDetachedCase) {
	t.Helper()
	directory := canonicalWorkspaceTestDir(t)
	runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
		if !slices.Equal(command.Args[3:], []string{"symbolic-ref", "-q", "HEAD"}) {
			t.Fatalf("unexpected Git command: %v", command.Args[3:])
		}
		return writeTestStreams(command, test.stdout, test.stderr, test.runErr)
	}}
	err := (Git{Runner: runner}).requireDetached(context.Background(), directory)
	if test.wantClean {
		if err != nil {
			t.Fatalf("clean detached HEAD: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("HEAD attachment fault was accepted as detached")
	}
	for _, want := range test.wantErr {
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	}
}

func writeTestStreams(command *exec.Cmd, stdout, stderr string, runErr error) error {
	if err := writeCommandOutput(command, stdout); err != nil {
		return err
	}
	if _, err := command.Stderr.Write([]byte(stderr)); err != nil {
		return err
	}
	return runErr
}

// symbolicRefAnswer is one fake native symbolic-ref --quiet --no-recurse
// result for the pinned owned ref.
type symbolicRefAnswer struct {
	stdout string
	stderr string
	err    error
}

var notSymbolicAnswer = symbolicRefAnswer{err: gitTestExitError(1)}

type exactRefPinCase struct {
	name       string
	ref        string
	before     symbolicRefAnswer
	inventory  string
	wantErr    []error
	wantCreate bool
	wantNoGit  bool
}

func TestPinBaseObservesExactOwnedRefBeforeCreateOnlyCAS(t *testing.T) {
	const ref = "refs/delegate/root/workspaces/one/base"
	tests := []exactRefPinCase{
		{name: "absent", before: notSymbolicAnswer, wantCreate: true},
		{name: "direct same", before: notSymbolicAnswer, inventory: testBaseOID},
		{name: "direct other", before: notSymbolicAnswer, inventory: testHeadOID, wantErr: []error{ErrRefConflict}},
		{name: "live symbolic", before: symbolicRefAnswer{stdout: ref + "-live\n"}, wantErr: []error{ErrRefUnreadable}},
		{name: "dangling foreign symbolic", before: symbolicRefAnswer{stdout: "refs/heads/deleted\n"}, wantErr: []error{ErrRefUnreadable}},
		{name: "invalid owned ref input", ref: "refs/heads/base", wantErr: []error{ErrUnownedRef}, wantNoGit: true},
		{
			name:    "not symbolic with stdout",
			before:  symbolicRefAnswer{stdout: "refs/heads/main\n", err: gitTestExitError(1)},
			wantErr: []error{ErrRefUnreadable, errUnexpectedSymbolicRefOutput, gitTestExitError(1)},
		},
		{
			name:    "not symbolic with stderr",
			before:  symbolicRefAnswer{stderr: "warning: ref storage\n", err: gitTestExitError(1)},
			wantErr: []error{ErrRefUnreadable, errUnexpectedSymbolicRefOutput, gitTestExitError(1)},
		},
		{name: "empty symbolic target", before: symbolicRefAnswer{}, wantErr: []error{ErrRefUnreadable, errMalformedSymbolicRefTarget}},
		{name: "fatal exit", before: symbolicRefAnswer{err: gitTestExitError(128)}, wantErr: []error{ErrRefUnreadable, gitTestExitError(128)}},
		{name: "signal exit", before: symbolicRefAnswer{err: gitTestExitError(-1)}, wantErr: []error{ErrRefUnreadable, gitTestExitError(-1)}},
		{
			name:    "context with joined close",
			before:  symbolicRefAnswer{err: errors.Join(gitTestExitError(1), context.DeadlineExceeded, errTestJoinedStreamClose)},
			wantErr: []error{ErrRefUnreadable, context.DeadlineExceeded, errTestJoinedStreamClose},
		},
		{
			name:    "output limit",
			before:  symbolicRefAnswer{stdout: "refs/heads/" + strings.Repeat("x", 64*1024) + "\n"},
			wantErr: []error{ErrRefUnreadable},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.ref == "" {
				test.ref = ref
			}
			runExactRefPinCase(t, test)
		})
	}
}

func runExactRefPinCase(t *testing.T, test exactRefPinCase) {
	t.Helper()
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	runner := newRepositoryFixtureRunner(root, "", "", testBaseOID)
	state := &exactRefState{ref: test.ref, objectID: testBaseOID, symbolic: test.before, inventory: test.inventory}
	runner.handler = state.handler(runner.handler)
	err := (Git{Runner: runner}).PinBase(context.Background(), source, test.ref)
	switch {
	case test.wantErr == nil && err != nil:
		t.Fatalf("pin: %v", err)
	case test.wantErr != nil && err == nil:
		t.Fatal("pin unexpectedly succeeded")
	}
	for _, want := range test.wantErr {
		if !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
	}
	if state.updates != boolCount(test.wantCreate) {
		t.Fatalf("update-ref calls = %d, want create=%v", state.updates, test.wantCreate)
	}
	if test.wantNoGit != (len(runner.calls) == 0) {
		t.Fatalf("Git launches = %d, want none=%v", len(runner.calls), test.wantNoGit)
	}
	if !test.wantNoGit && !containsArgs(runner.calls, []string{"symbolic-ref", "--quiet", "--no-recurse", test.ref}) {
		t.Fatal("pin did not observe the exact owned ref")
	}
}

// exactRefState is one fake owned ref: its symbolic-ref answer, inventory OID,
// and an optional answer after a failed create-only CAS.
type exactRefState struct {
	ref           string
	objectID      string
	symbolic      symbolicRefAnswer
	inventory     string
	casErr        error
	afterSymbolic *symbolicRefAnswer
	updates       int
}

func (s *exactRefState) handler(base func(*exec.Cmd) error) func(*exec.Cmd) error {
	return func(command *exec.Cmd) error {
		args := command.Args[3:]
		switch {
		case slices.Equal(args, []string{"symbolic-ref", "--quiet", "--no-recurse", s.ref}):
			return writeTestStreams(command, s.symbolic.stdout, s.symbolic.stderr, s.symbolic.err)
		case slices.Equal(args, []string{"for-each-ref", "--format=" + gitOwnedRefFormat, s.ref}):
			if s.inventory == "" {
				return nil
			}
			return writeCommandOutput(command, string(refInventoryRecord(s.ref, s.inventory, "commit", "")))
		case len(args) == 5 && slices.Equal(args[:3], []string{"update-ref", "--no-deref", s.ref}):
			s.updates++
			if s.casErr != nil {
				if s.afterSymbolic != nil {
					s.symbolic = *s.afterSymbolic
				}
				return s.casErr
			}
			s.inventory = s.objectID
			return nil
		}
		return base(command)
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

// A foreign writer can race between the observation and the CAS; the reread
// after a failed CAS uses the same observer and keeps the CAS cause.
func TestPinBaseRereadsSymbolicWinnerAfterFailedCAS(t *testing.T) {
	const ref = "refs/delegate/root/workspaces/one/base"
	root := canonicalWorkspaceTestDir(t)
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := Source{Root: root, CommonDir: gitDir, GitDir: gitDir, RelativeCwd: ".", BaseOID: testBaseOID, ObjectFormat: "sha1"}
	runner := newRepositoryFixtureRunner(root, "", "", testBaseOID)
	winner := symbolicRefAnswer{stdout: "refs/heads/deleted\n"}
	state := &exactRefState{ref: ref, objectID: testBaseOID, symbolic: notSymbolicAnswer, casErr: gitTestExitError(128), afterSymbolic: &winner}
	runner.handler = state.handler(runner.handler)
	err := (Git{Runner: runner}).PinBase(context.Background(), source, ref)
	if !errors.Is(err, ErrRefUnreadable) || !errors.Is(err, gitTestExitError(128)) {
		t.Fatalf("error = %v, want ErrRefUnreadable with the CAS cause", err)
	}
	if state.updates != 1 {
		t.Fatalf("update-ref calls = %d, want exactly one", state.updates)
	}
}

// PinCapture and PinTurnBaseline keep their own validations and converge on
// the same exact observation before publication.
func TestCaptureAndBaselinePinsRefuseSymbolicOwnedRef(t *testing.T) {
	const ref = "refs/delegate/root/workspaces/one/capture"
	pins := map[string]func(Git, Checkout) error{
		"capture": func(git Git, checkout Checkout) error {
			candidate := CaptureCandidate{BaseOID: testBaseOID, ObservedHEAD: testHeadOID, TreeOID: testTreeOID, ObjectFormat: "sha1"}
			return git.PinCapture(context.Background(), checkout, ref, candidate)
		},
		"baseline": func(git Git, checkout Checkout) error {
			baseline := TurnBaseline{BaseOID: testBaseOID, ObservedHEAD: testHeadOID, TreeOID: testTreeOID, ObjectFormat: "sha1", Ref: ref}
			return git.PinTurnBaseline(context.Background(), checkout, baseline)
		},
	}
	for name, pin := range pins {
		t.Run(name, func(t *testing.T) {
			fixture := newCheckoutFixture(t)
			runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
			state := &exactRefState{ref: ref, objectID: testTreeOID, symbolic: symbolicRefAnswer{stdout: "refs/heads/deleted\n"}}
			runner.handler = state.handler(runner.handler)
			err := pin(Git{Runner: runner}, fixture.checkout)
			if !errors.Is(err, ErrRefUnreadable) {
				t.Fatalf("error = %v, want ErrRefUnreadable", err)
			}
			if state.updates != 0 {
				t.Fatalf("symbolic owned ref reached update-ref %d times", state.updates)
			}
			if !containsArgs(runner.calls, []string{"rev-parse", "--verify", "--end-of-options", testTreeOID + "^{tree}"}) {
				t.Fatal("pin skipped its tree object validation")
			}
		})
	}
}
