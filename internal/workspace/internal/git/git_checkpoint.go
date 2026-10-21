package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// Every checkpoint commit has the same author, committer, timestamp and
// subject. Fixed values keep its payload independent of user Git identity,
// signing configuration and the clock; the trailers carry its identity.
const (
	checkpointSignature = "Delegate <delegate@localhost> 1761048000 +0000"
	checkpointSubject   = "Delegate checkpoint"
)

// ResolveOrdinaryCheckpointTarget resolves the ordinary checkout containing
// cwd as its own source. The checkout root is Git's top level and the admin
// directory is Git's actual per-worktree directory, for a main or linked
// worktree alike. cwd must be the caller's canonical selection and exactly the
// directory Git resolves. The source records HEAD as its base and is not yet
// pinned: the caller pins its common-directory identity at the application
// edge before PrepareOrdinaryCheckpoint.
func (g Git) ResolveOrdinaryCheckpointTarget(ctx context.Context, cwd string) (contract.CheckpointTarget, error) {
	source, err := g.ResolveSource(ctx, cwd, "")
	if err != nil {
		return contract.CheckpointTarget{}, err
	}
	if filepath.Join(source.Root, filepath.FromSlash(source.RelativeCwd)) != cwd {
		return contract.CheckpointTarget{}, fmt.Errorf("%w: checkpoint directory is not the canonical resolved directory", contract.ErrIdentityMismatch)
	}
	return ordinaryCheckpointTarget(source), nil
}

func ordinaryCheckpointTarget(source contract.Source) contract.CheckpointTarget {
	return contract.CheckpointTarget{Source: source, CheckoutRoot: source.Root, AdminDir: source.GitDir}
}

// PrepareOrdinaryCheckpoint prepares the candidate for a pinned ordinary
// target. The checkout must still be the same source repository; content is
// read from its own root and admin directory.
func (g Git) PrepareOrdinaryCheckpoint(ctx context.Context, request contract.OperationRequest, requestSHA256 string, target contract.CheckpointTarget, scratchDir string) (contract.CheckpointCandidate, error) {
	if ctx == nil {
		return contract.CheckpointCandidate{}, errors.Join(contract.ErrInvalidGitContext, errors.New("nil checkpoint context"))
	}
	if err := checkpointRequestSelects(request, requestSHA256, contract.CheckpointOrdinary); err != nil {
		return contract.CheckpointCandidate{}, err
	}
	if target != ordinaryCheckpointTarget(target.Source) {
		return contract.CheckpointCandidate{}, fmt.Errorf("%w: ordinary checkpoint target is not its own source checkout", contract.ErrIdentityMismatch)
	}
	if err := g.validateSourceIdentity(ctx, target.Source); err != nil {
		return contract.CheckpointCandidate{}, err
	}
	source := target.Source
	scratch, err := validateScratchDirectory(scratchDir, "checkpoint", source.Root, source.CommonDir, source.GitDir)
	if err != nil {
		return contract.CheckpointCandidate{}, err
	}
	return g.prepareCheckpoint(ctx, request, requestSHA256, target, scratch)
}

// PrepareManagedCheckpoint prepares the candidate for an existing managed
// checkout after its full topology validation. The target keeps the original
// source provenance and records the checkout's own root and admin directory.
func (g Git) PrepareManagedCheckpoint(ctx context.Context, request contract.OperationRequest, requestSHA256 string, checkout contract.Checkout, scratchDir string) (contract.CheckpointCandidate, error) {
	if ctx == nil {
		return contract.CheckpointCandidate{}, errors.Join(contract.ErrInvalidGitContext, errors.New("nil checkpoint context"))
	}
	if err := checkpointRequestSelects(request, requestSHA256, contract.CheckpointManaged); err != nil {
		return contract.CheckpointCandidate{}, err
	}
	if err := g.validateCheckout(ctx, checkout); err != nil {
		return contract.CheckpointCandidate{}, err
	}
	scratch, err := validateScratchDirectory(scratchDir, "checkpoint", append(checkoutContextPaths(checkout), checkout.AdminDir)...)
	if err != nil {
		return contract.CheckpointCandidate{}, err
	}
	target := contract.CheckpointTarget{Source: checkout.Source, CheckoutRoot: checkout.CheckoutPath, AdminDir: checkout.AdminDir}
	return g.prepareCheckpoint(ctx, request, requestSHA256, target, scratch)
}

// checkpointRequestSelects refuses, before any Git command, a request that is
// not a checkpoint creation of kind or whose identities cannot form the owned
// ref and payload trailers.
func checkpointRequestSelects(request contract.OperationRequest, requestSHA256, kind string) error {
	if request.Kind != contract.OperationCheckpoint || request.Checkpoint == nil || request.Checkpoint.Kind != kind {
		return fmt.Errorf("%w: request is not a %s checkpoint creation", contract.ErrIdentityMismatch, kind)
	}
	if _, err := contract.CheckpointRef(request.RootID, request.OperationID); err != nil {
		return err
	}
	return task.ValidateSHA256(requestSHA256)
}

// prepareCheckpoint observes HEAD, builds the full current tree through the
// shared private-index builder and hashes the exact commit payload without
// writing a commit or ref; the builder's tree and blob objects stay
// unreferenced until publication. The target's repository identity and the
// scratch directory are already validated. A candidate is returned only after
// the contract accepts it, alongside any later scratch-removal error.
func (g Git) prepareCheckpoint(ctx context.Context, request contract.OperationRequest, requestSHA256 string, target contract.CheckpointTarget, scratch string) (contract.CheckpointCandidate, error) {
	source := target.Source
	if err := g.verifyObject(ctx, source.CommonDir, source.BaseOID, source.ObjectFormat, "commit"); err != nil {
		return contract.CheckpointCandidate{}, fmt.Errorf("validate checkpoint source base: %w", err)
	}
	observedHEAD, err := g.runScalar(ctx, target.CheckoutRoot, []string{"rev-parse", "--verify", "--end-of-options", "HEAD^{commit}"}, nil)
	if err != nil {
		return contract.CheckpointCandidate{}, fmt.Errorf("observe checkpoint HEAD: %w", err)
	}
	if err = contract.ValidateOID(observedHEAD, source.ObjectFormat); err != nil {
		return contract.CheckpointCandidate{}, fmt.Errorf("observe checkpoint HEAD: %w", err)
	}
	content := captureTarget{root: target.CheckoutRoot, adminDir: target.AdminDir, objectFormat: source.ObjectFormat}
	treeOID, indexDigest, buildErr := g.buildCaptureContent(ctx, content, scratch)
	if treeOID == "" {
		return contract.CheckpointCandidate{}, buildErr
	}
	payload := checkpointCommitPayload(treeOID, observedHEAD, request.OperationID, requestSHA256)
	commitOID, err := g.hashCommitObject(ctx, target.CheckoutRoot, payload, source.ObjectFormat, false)
	if err != nil {
		return contract.CheckpointCandidate{}, errors.Join(buildErr, err)
	}
	ref, err := contract.CheckpointRef(request.RootID, request.OperationID)
	if err != nil {
		return contract.CheckpointCandidate{}, errors.Join(buildErr, err)
	}
	candidate := contract.CheckpointCandidate{
		SchemaVersion: task.SchemaVersion, OperationID: request.OperationID, RequestSHA256: requestSHA256, Target: target,
		ObservedHEAD: observedHEAD, IndexSHA256: indexDigest, TreeOID: treeOID, CommitOID: commitOID, SnapshotRef: ref, CommitPayload: payload,
	}
	if err = contract.ValidateCheckpointCandidate(candidate, request, requestSHA256); err != nil {
		return contract.CheckpointCandidate{}, errors.Join(buildErr, err)
	}
	return candidate, buildErr
}

// checkpointCommitPayload is the exact commit object body: the full tree, the
// observed HEAD as sole parent, the fixed signatures, and the checkpoint ID
// and request digest trailers as the final message lines.
func checkpointCommitPayload(treeOID, parentOID, checkpointID, requestSHA256 string) string {
	return "tree " + treeOID + "\nparent " + parentOID +
		"\nauthor " + checkpointSignature + "\ncommitter " + checkpointSignature + "\n\n" + checkpointSubject + "\n\n" +
		contract.CheckpointTrailer + checkpointID + "\n" + contract.CheckpointRequestTrailer + requestSHA256 + "\n"
}

// hashCommitObject asks native Git for the commit object ID of payload in the
// repository's object format. Only write stores the object; no ref changes.
func (g Git) hashCommitObject(ctx context.Context, directory, payload, objectFormat string, write bool) (string, error) {
	args := []string{"hash-object", "-t", "commit", "--stdin"}
	if write {
		args = []string{"hash-object", "-t", "commit", "-w", "--stdin"}
	}
	stdout, stderr, err := g.runOutput(ctx, directory, args, nil, strings.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("hash Git checkpoint commit: %w", gitCommandError(err, stderr))
	}
	oid := removeOneTerminatingNewline(string(stdout))
	if err = contract.ValidateOID(oid, objectFormat); err != nil {
		return "", fmt.Errorf("hash Git checkpoint commit: %w", err)
	}
	return oid, nil
}

// PublishCheckpoint writes the journal-persisted candidate's exact stored
// payload and pins its owned ref create-only. The candidate must bind to its
// creation request and digest. The written object must hash to the recorded
// commit before any ref is observed or created; the payload is never rebuilt.
// Only the shared common directory is required, so a transient source checkout
// may be gone. The caller verifies the pinned common-directory identity first.
// No branch, HEAD or index changes.
func (g Git) PublishCheckpoint(ctx context.Context, request contract.OperationRequest, requestSHA256 string, candidate contract.CheckpointCandidate) error {
	if ctx == nil {
		return errors.Join(contract.ErrInvalidGitContext, errors.New("nil checkpoint publication context"))
	}
	if err := contract.ValidateCheckpointCandidate(candidate, request, requestSHA256); err != nil {
		return err
	}
	source := candidate.Target.Source
	if err := g.validateRepositoryIdentity(ctx, source); err != nil {
		return err
	}
	if err := g.verifyObject(ctx, source.CommonDir, candidate.TreeOID, source.ObjectFormat, "tree"); err != nil {
		return fmt.Errorf("validate checkpoint tree object: %w", err)
	}
	if err := g.verifyObject(ctx, source.CommonDir, candidate.ObservedHEAD, source.ObjectFormat, "commit"); err != nil {
		return fmt.Errorf("validate checkpoint parent object: %w", err)
	}
	written, err := g.hashCommitObject(ctx, source.CommonDir, candidate.CommitPayload, source.ObjectFormat, true)
	if err != nil {
		return err
	}
	if !strings.EqualFold(written, candidate.CommitOID) {
		return fmt.Errorf("%w: stored checkpoint payload hashes to %s, want %s", contract.ErrIdentityMismatch, written, candidate.CommitOID)
	}
	return g.pinCreateOnly(ctx, source.CommonDir, candidate.SnapshotRef, candidate.CommitOID, source.ObjectFormat, "commit", "pin Git checkpoint ref")
}

// RemoveCheckpoint deletes the checkpoint's canonical owned ref in its pinned
// common directory only while it still names the recorded commit. It reports
// whether the ref was present; a ref proven absent is not an error. It never
// needs the source checkout. The caller verifies the pinned common-directory
// identity first, which refuses a replaced repository; a missing common
// directory, or a symbolic, unreadable or conflicting ref, is refused here.
func (g Git) RemoveCheckpoint(ctx context.Context, rootID string, candidate contract.CheckpointCandidate) (removed bool, err error) {
	if ctx == nil {
		return false, errors.Join(contract.ErrInvalidGitContext, errors.New("nil checkpoint removal context"))
	}
	if err = contract.ValidateCheckpointRef(candidate.SnapshotRef, rootID, candidate.OperationID); err != nil {
		return false, err
	}
	source := candidate.Target.Source
	if _, pinned := source.CommonDirIdentity(); !pinned {
		return false, fmt.Errorf("%w: checkpoint common directory identity is not pinned", contract.ErrIdentityMismatch)
	}
	if err = contract.ValidateOID(candidate.CommitOID, source.ObjectFormat); err != nil {
		return false, err
	}
	if err = g.validateRepositoryIdentity(ctx, source); err != nil {
		return false, err
	}
	_, observeErr := g.lookupRef(ctx, source.CommonDir, candidate.SnapshotRef, source.ObjectFormat)
	if observeErr != nil && !errors.Is(observeErr, errRefAbsent) {
		return false, observeErr
	}
	if err = g.DeleteRef(ctx, source, candidate.SnapshotRef, candidate.CommitOID); err != nil {
		return false, err
	}
	return observeErr == nil, nil
}
