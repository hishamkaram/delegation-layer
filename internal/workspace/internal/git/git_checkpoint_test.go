package git

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

const (
	testCommitOID         = "5555555555555555555555555555555555555555"
	testCheckpointRootID  = "11111111111111111111111111111111"
	testCheckpointID      = "22222222222222222222222222222222"
	testCheckpointRequest = "6666666666666666666666666666666666666666666666666666666666666666"
)

var (
	checkpointHashArgs  = []string{"hash-object", "-t", "commit", "--stdin"}
	checkpointWriteArgs = []string{"hash-object", "-t", "commit", "-w", "--stdin"}
)

func checkpointTestRequest(kind string) contract.OperationRequest {
	request := contract.OperationRequest{SchemaVersion: task.SchemaVersion, RootID: testCheckpointRootID, OperationID: testCheckpointID, Kind: contract.OperationCheckpoint}
	request.Checkpoint = &contract.CheckpointSelection{Kind: kind}
	if kind == contract.CheckpointManaged {
		request.WorkspaceID = strings.Repeat("3", 32)
	}
	return request
}

// managedCheckpointFixture pins the capture fixture's common directory, as a
// managed source records it, and answers the commit hash with testCommitOID
// after recording each payload Git received on stdin.
func managedCheckpointFixture(t *testing.T) (checkoutFixture, *recordingGitRunner, *[]string) {
	t.Helper()
	fixture := newCheckoutFixture(t)
	fixture.checkout.Source = fixture.checkout.Source.WithCommonDirIdentity(contract.DirectoryIdentity{Device: 1, Inode: 2})
	runner := captureRunner(fixture, "100644 "+testBlobOID+" 0\tstaged\x00")
	payloads := &[]string{}
	inner := runner.handler
	runner.handler = func(command *exec.Cmd) error {
		args := command.Args[3:]
		if slices.Equal(args, checkpointHashArgs) || slices.Equal(args, checkpointWriteArgs) {
			data, err := io.ReadAll(command.Stdin)
			if err != nil {
				return err
			}
			*payloads = append(*payloads, string(data))
			return writeCommandOutput(command, testCommitOID+"\n")
		}
		return inner(command)
	}
	return fixture, runner, payloads
}

// Preparation hashes the exact bounded payload, bound to the built tree, the
// observed HEAD and both trailers, without -w, and never touches a ref.
func TestPrepareManagedCheckpointHashesExactPayloadWithoutWriting(t *testing.T) {
	fixture, runner, payloads := managedCheckpointFixture(t)
	request := checkpointTestRequest(contract.CheckpointManaged)
	candidate, err := (Git{Runner: runner}).PrepareManagedCheckpoint(context.Background(), request, testCheckpointRequest, fixture.checkout, fixture.scratch)
	if err != nil {
		t.Fatal(err)
	}
	want := "tree " + testTreeOID + "\nparent " + testHeadOID +
		"\nauthor Delegate <delegate@localhost> 1761048000 +0000\ncommitter Delegate <delegate@localhost> 1761048000 +0000\n\nDelegate checkpoint\n\n" +
		contract.CheckpointTrailer + testCheckpointID + "\n" + contract.CheckpointRequestTrailer + testCheckpointRequest + "\n"
	if candidate.CommitPayload != want || !slices.Equal(*payloads, []string{want}) {
		t.Fatalf("payload = %q, hashed %q; want exactly %q", candidate.CommitPayload, *payloads, want)
	}
	if candidate.CommitOID != testCommitOID || candidate.TreeOID != testTreeOID || candidate.ObservedHEAD != testHeadOID ||
		candidate.Target.Source.BaseOID != testBaseOID || candidate.Target.CheckoutRoot != fixture.checkout.CheckoutPath || candidate.Target.AdminDir != fixture.checkout.AdminDir {
		t.Fatalf("candidate = %+v", candidate)
	}
	for _, command := range runner.calls {
		args := command.Args[3:]
		if slices.Equal(args, checkpointWriteArgs) || slices.Contains(args, "update-ref") || slices.Contains(args, "symbolic-ref") {
			t.Fatalf("preparation ran %v", args)
		}
	}
	assertCaptureScratchEmpty(t, fixture.scratch)
}

// A verified candidate stays returned alongside a scratch-removal error; a
// failed commit hash returns no candidate even though the tree was verified.
func TestPrepareManagedCheckpointKeepsOnlyVerifiedCandidates(t *testing.T) {
	t.Run("scratch removal", func(t *testing.T) {
		fixture, runner, _ := managedCheckpointFixture(t)
		privateIndex := holdPrivateIndexAtWriteTree(t, runner)
		candidate, err := (Git{Runner: runner}).PrepareManagedCheckpoint(context.Background(), checkpointTestRequest(contract.CheckpointManaged), testCheckpointRequest, fixture.checkout, fixture.scratch)
		var removeErr *os.PathError
		if !errors.As(err, &removeErr) || removeErr.Op != "remove" || removeErr.Path != *privateIndex {
			t.Fatalf("checkpoint error = %v, want the private index removal failure", err)
		}
		if validateErr := contract.ValidateCheckpointCandidate(candidate, checkpointTestRequest(contract.CheckpointManaged), testCheckpointRequest); validateErr != nil || candidate.CommitOID != testCommitOID {
			t.Fatalf("candidate = %+v (%v), want the verified candidate alongside the removal error", candidate, validateErr)
		}
	})
	t.Run("hash failure", func(t *testing.T) {
		fixture, runner, _ := managedCheckpointFixture(t)
		inner := runner.handler
		runner.handler = func(command *exec.Cmd) error {
			if slices.Equal(command.Args[3:], checkpointHashArgs) {
				return gitTestExitError(128)
			}
			return inner(command)
		}
		candidate, err := (Git{Runner: runner}).PrepareManagedCheckpoint(context.Background(), checkpointTestRequest(contract.CheckpointManaged), testCheckpointRequest, fixture.checkout, fixture.scratch)
		if err == nil || candidate != (contract.CheckpointCandidate{}) {
			t.Fatalf("checkpoint with a failed hash = %+v, %v; want a zero candidate and the failure", candidate, err)
		}
		assertCaptureScratchEmpty(t, fixture.scratch)
	})
}

// holdPrivateIndexAtWriteTree makes the private index path an unremovable
// nonempty directory when the tree is written and returns that path.
func holdPrivateIndexAtWriteTree(t *testing.T, runner *recordingGitRunner) *string {
	t.Helper()
	inner := runner.handler
	privateIndex := new(string)
	runner.handler = func(command *exec.Cmd) error {
		if slices.Equal(command.Args[3:], []string{"write-tree"}) {
			*privateIndex = indexOverride(command.Env)
			if err := os.MkdirAll(filepath.Join(*privateIndex, "held"), 0o700); err != nil {
				return err
			}
		}
		return inner(command)
	}
	return privateIndex
}

// Preparation refuses a request of the other kind, identities that cannot
// form the owned ref or trailers, a misplaced scratch directory, and an
// ordinary target that is not its own source, before any Git command.
func TestPrepareCheckpointRefusesBeforeAnyGitCommand(t *testing.T) {
	fixture, runner, _ := managedCheckpointFixture(t)
	source := fixture.checkout.Source
	ordinary := contract.CheckpointTarget{Source: source, CheckoutRoot: source.Root, AdminDir: fixture.checkout.AdminDir}
	git := Git{Runner: runner}
	ctx := context.Background()
	badRoot := checkpointTestRequest(contract.CheckpointManaged)
	badRoot.RootID = "root"
	cases := map[string]func() error{
		"ordinary request for managed checkout": func() error {
			_, err := git.PrepareManagedCheckpoint(ctx, checkpointTestRequest(contract.CheckpointOrdinary), testCheckpointRequest, fixture.checkout, fixture.scratch)
			return err
		},
		"managed request for ordinary target": func() error {
			_, err := git.PrepareOrdinaryCheckpoint(ctx, checkpointTestRequest(contract.CheckpointManaged), testCheckpointRequest, ordinary, fixture.scratch)
			return err
		},
		"invalid root": func() error {
			_, err := git.PrepareManagedCheckpoint(ctx, badRoot, testCheckpointRequest, fixture.checkout, fixture.scratch)
			return err
		},
		"invalid request digest": func() error {
			_, err := git.PrepareManagedCheckpoint(ctx, checkpointTestRequest(contract.CheckpointManaged), "digest", fixture.checkout, fixture.scratch)
			return err
		},
		"ordinary admin is not its git dir": func() error {
			_, err := git.PrepareOrdinaryCheckpoint(ctx, checkpointTestRequest(contract.CheckpointOrdinary), testCheckpointRequest, ordinary, fixture.scratch)
			return err
		},
	}
	for name, prepare := range cases {
		if err := prepare(); err == nil {
			t.Errorf("%s: preparation succeeded", name)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("refused preparations ran %d Git commands", len(runner.calls))
	}
}

// A scratch directory inside the checkout's admin directory is refused with
// the shared guard's error identity.
func TestPrepareManagedCheckpointRefusesScratchInsideGitContext(t *testing.T) {
	fixture, runner, _ := managedCheckpointFixture(t)
	_, err := (Git{Runner: runner}).PrepareManagedCheckpoint(context.Background(), checkpointTestRequest(contract.CheckpointManaged), testCheckpointRequest, fixture.checkout, fixture.checkout.AdminDir)
	if !errors.Is(err, ErrInvalidGitContext) || !strings.Contains(err.Error(), "checkpoint scratch directory overlaps Git context") {
		t.Fatalf("scratch inside the admin directory: %v", err)
	}
}

// Publication writes the exact stored payload and refuses a written object
// whose ID differs from the candidate before observing or creating any ref.
func TestPublishCheckpointRefusesHashMismatchBeforeAnyRefCommand(t *testing.T) {
	fixture, runner, payloads := managedCheckpointFixture(t)
	request := checkpointTestRequest(contract.CheckpointManaged)
	git := Git{Runner: runner}
	candidate, err := git.PrepareManagedCheckpoint(context.Background(), request, testCheckpointRequest, fixture.checkout, fixture.scratch)
	if err != nil {
		t.Fatal(err)
	}
	candidate.CommitOID = strings.Repeat("7", 40)
	runner.calls, *payloads = nil, nil
	err = git.PublishCheckpoint(context.Background(), request, testCheckpointRequest, candidate)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("publish with a mismatched commit ID: %v, want ErrIdentityMismatch", err)
	}
	if !slices.Equal(*payloads, []string{candidate.CommitPayload}) || !containsArgs(runner.calls, checkpointWriteArgs) {
		t.Fatalf("publication wrote %q, want exactly the stored payload", *payloads)
	}
	for _, command := range runner.calls {
		if args := command.Args[3:]; slices.Contains(args, "update-ref") || slices.Contains(args, "symbolic-ref") || slices.Contains(args, "for-each-ref") {
			t.Fatalf("refused publication ran ref command %v", args)
		}
	}
}

// Removal refuses an unpinned common directory, a ref outside the canonical
// checkpoint name, and a malformed expected commit, before any Git command.
func TestRemoveCheckpointRefusesUnverifiableInputsBeforeAnyGitCommand(t *testing.T) {
	fixture, runner, _ := managedCheckpointFixture(t)
	ref, err := contract.CheckpointRef(testCheckpointRootID, testCheckpointID)
	if err != nil {
		t.Fatal(err)
	}
	valid := contract.CheckpointCandidate{OperationID: testCheckpointID, SnapshotRef: ref, CommitOID: testCommitOID, Target: contract.CheckpointTarget{Source: fixture.checkout.Source}}
	cases := map[string]func(*contract.CheckpointCandidate){
		"unpinned common directory": func(candidate *contract.CheckpointCandidate) { candidate.Target.Source.CommonDirInode = 0 },
		"foreign ref":               func(candidate *contract.CheckpointCandidate) { candidate.SnapshotRef = ref + "-other" },
		"malformed commit":          func(candidate *contract.CheckpointCandidate) { candidate.CommitOID = "HEAD" },
	}
	for name, alter := range cases {
		candidate := valid
		alter(&candidate)
		if removed, removeErr := (Git{Runner: runner}).RemoveCheckpoint(context.Background(), testCheckpointRootID, candidate); removed || removeErr == nil {
			t.Errorf("%s: removal = %v, %v; want a refusal", name, removed, removeErr)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("refused removals ran %d Git commands", len(runner.calls))
	}
}
