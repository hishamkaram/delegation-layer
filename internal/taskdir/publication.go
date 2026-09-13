package taskdir

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func (td *TaskDir) withCollection(fn func() (*task.OutcomeRecord, error, error)) (out *task.OutcomeRecord, cleanupErr, resultErr error) {
	if err := td.store.maintLock.LockSHNonblocking(); err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.maintLock.Unlock()) }()
	if err := td.runLock.LockEXNonblocking(); err != nil {
		return nil, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.runLock.Unlock()) }()
	return fn()
}

var errNoOutcome = errors.New("outcome absent")

func (td *TaskDir) readWinner(meta *task.MetaRecord, specHash, metaHash string) (*task.OutcomeRecord, error) {
	out, err := td.readWinnerRecord(meta, specHash, metaHash)
	if err != nil && !errors.Is(err, errNoOutcome) {
		return nil, fmt.Errorf("%w: %w", task.ErrInvariantFault, err)
	}
	return out, err
}

func (td *TaskDir) readWinnerRecord(meta *task.MetaRecord, specHash, metaHash string) (*task.OutcomeRecord, error) {
	var outcome task.OutcomeRecord
	if err := td.store.readRecord(filepath.Join(td.Dir, "outcome.json"), &outcome); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoOutcome
		}
		return nil, err
	}
	if err := task.ValidateOutcomeRecord(&outcome); err != nil {
		return nil, err
	}
	if outcome.RootID != td.store.RootID || outcome.TaskID != td.TaskID || outcome.SpecSHA256 != specHash || outcome.MetaSHA256 != metaHash {
		return nil, task.ErrIdentityMismatch
	}
	if !outcome.Predicate.Equal(meta.Predicate) {
		return nil, task.ErrIncompatiblePredicate
	}
	digest, size, err := td.store.fileDigest(filepath.Join(td.Dir, outcome.Payload.Basename))
	if err != nil {
		return nil, err
	}
	if digest != outcome.Payload.SHA256 || size != outcome.Payload.Length {
		return nil, task.ErrEvidenceFault
	}
	_, evidence, err := td.rawManifest()
	if err != nil {
		return nil, err
	}
	if outcome.EvidenceSHA256 != evidence {
		return nil, task.ErrEvidenceFault
	}
	return &outcome, nil
}

func (td *TaskDir) acknowledgeWinner(outcome *task.OutcomeRecord) (*task.OutcomeRecord, error) {
	return outcome, td.acknowledgeRecord("outcome.json")
}

// Visibility alone does not establish durability after a failed publication.
// Repeat the exact destination's directory barrier before acknowledging it.
func (td *TaskDir) acknowledgeRecord(name string) error {
	path := filepath.Join(td.Dir, name)
	if inj := td.store.faultInjector; inj != nil {
		if err := inj.OnPostLinkDirBarrier(path); err != nil {
			return fmt.Errorf("%w: %w", task.ErrUncertainDurability, err)
		}
	}
	if err := td.store.barrierDir(td.Dir); err != nil {
		return fmt.Errorf("%w: %w", task.ErrUncertainDurability, err)
	}
	if inj := td.store.faultInjector; inj != nil {
		return inj.OnAfterLinkDirBarrier(path)
	}
	return nil
}

// Collect only publishes the registered pure predicate's decision after validating
// the complete immutable evidence set. It can never acquire Start authority.
func (td *TaskDir) Collect(predicate task.PredicateRef) (*task.OutcomeRecord, error, error) {
	return td.withCollection(func() (*task.OutcomeRecord, error, error) { return td.collectOwned(predicate) })
}

// Finalize retains the consumed runner's maintenance/run lease through pure
// publication. It never reacquires or exposes a way to bypass collection locks.
func (td *TaskDir) Finalize(predicate task.PredicateRef) (*task.OutcomeRecord, error, error) {
	state, err := td.ownedRunner()
	if err != nil {
		return nil, nil, err
	}
	defer state.mu.Unlock()
	if !state.sealed || state.activeWriters != 0 || state.poisoned != nil {
		return nil, nil, task.ErrEvidenceFault
	}
	return td.collectOwned(predicate)
}

func (td *TaskDir) collectOwned(predicate task.PredicateRef) (*task.OutcomeRecord, error, error) {
	_, _, meta, spec, metaHash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, nil, err
	}
	if !predicate.Equal(meta.Predicate) {
		return nil, nil, task.ErrIncompatiblePredicate
	}
	winner, err := td.readWinner(meta, spec, metaHash)
	if err == nil {
		winner, err = td.acknowledgeWinner(winner)
		return winner, nil, err
	}
	if !errors.Is(err, errNoOutcome) {
		return nil, nil, err
	}
	// Missing payload beneath an existing outcome is a fault, not permission to
	// publish another verdict.
	exists, err := td.store.exists(filepath.Join(td.Dir, "outcome.json"))
	if err != nil {
		return nil, nil, err
	}
	if exists {
		return nil, nil, task.ErrEvidenceFault
	}
	seal, err := td.readSeal(predicate, spec, metaHash)
	if errors.Is(err, task.ErrNoSeal) {
		return nil, nil, task.ErrNoSeal
	}
	if err != nil {
		return nil, nil, err
	}
	decision, err := task.EvaluateRegisteredPredicate(predicate, seal)
	if err != nil {
		return nil, nil, err
	}
	return td.publishDecision(decision, seal, spec, metaHash)
}

func (td *TaskDir) publishDecision(decision task.PredicateDecision, seal *task.ProviderExitRecord, spec, metaHash string) (*task.OutcomeRecord, error, error) {
	descriptor := task.PayloadDescriptor{Basename: decision.PayloadBasename, Length: int64(len(decision.PayloadContent)), SHA256: task.ComputeSHA256(decision.PayloadContent)}
	var reader io.Reader = bytes.NewReader(decision.PayloadContent)
	if decision.RawPath != "" {
		f, err := td.store.openFile(filepath.Join(td.Dir, decision.RawPath), unix.O_RDONLY)
		if err != nil {
			return nil, nil, err
		}
		defer closeFileQuietly(f)
		reader = f
		found := false
		for _, entry := range seal.RawManifest {
			if entry.Path == decision.RawPath {
				descriptor.Length = entry.Size
				descriptor.SHA256 = entry.SHA256
				found = true
			}
		}
		if !found {
			return nil, nil, task.ErrEvidenceFault
		}
	}
	candidate := task.OutcomeRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, SpecSHA256: spec, MetaSHA256: metaHash, Verdict: decision.Verdict, EvidenceSHA256: seal.ManifestSHA256, Predicate: seal.Predicate, Payload: descriptor}
	return td.publishCandidate(&candidate, reader)
}

func (td *TaskDir) publishCandidate(candidate *task.OutcomeRecord, reader io.Reader) (*task.OutcomeRecord, error, error) {
	if err := task.ValidateOutcomeRecord(candidate); err != nil {
		return nil, nil, err
	}
	data, err := marshalControlRecord(candidate)
	if err != nil {
		return nil, nil, err
	}
	if err = td.ensurePayload(candidate.Payload, reader); err != nil {
		return nil, nil, err
	}
	committed, cleanupErr, err := td.store.stageAndCommit(td.Dir, "outcome.json", data, td.store.faultInjector)
	if errors.Is(err, os.ErrExist) {
		_, _, meta, spec, metaHash, readErr := td.loadAndValidatePreparedSet()
		if readErr != nil {
			return nil, nil, readErr
		}
		winner, readErr := td.readWinner(meta, spec, metaHash)
		if readErr != nil {
			return nil, nil, readErr
		}
		winner, readErr = td.acknowledgeWinner(winner)
		if readErr != nil {
			return winner, nil, readErr
		}
		if !task.CompareOutcomes(candidate, winner) {
			return winner, nil, task.ErrOutcomeConflict
		}
		return winner, nil, nil
	}
	if err != nil {
		return nil, cleanupErr, err
	}
	if !committed {
		return nil, cleanupErr, task.ErrUncertainDurability
	}
	return candidate, cleanupErr, nil
}

func (td *TaskDir) ensurePayload(descriptor task.PayloadDescriptor, reader io.Reader) error {
	path := filepath.Join(td.Dir, descriptor.Basename)
	exists, err := td.store.exists(path)
	if err != nil {
		return fmt.Errorf("%w: %w", task.ErrInvariantFault, err)
	}
	if !exists {
		_, cleanupErr, stageErr := td.store.stageReader(td.Dir, descriptor.Basename, reader, td.store.faultInjector)
		if stageErr != nil && !errors.Is(stageErr, os.ErrExist) {
			return stageErr
		}
		if cleanupErr != nil {
			return cleanupErr
		}
	}
	digest, size, err := td.store.fileDigest(path)
	if err != nil {
		return fmt.Errorf("%w: %w", task.ErrInvariantFault, err)
	}
	if digest != descriptor.SHA256 || size != descriptor.Length {
		return task.ErrInvariantFault
	}
	return td.store.barrierDir(td.Dir)
}

// PublishCandidateForTest is the explicit adversarial fixture seam. Normal
// collectors use Collect; callers cannot override its predicate-derived result.
func (td *TaskDir) PublishCandidateForTest(verdict, basename string, content []byte, predicate task.PredicateRef) (*task.OutcomeRecord, error, error) {
	return td.withCollection(func() (*task.OutcomeRecord, error, error) {
		return td.candidateForTest(verdict, basename, content, predicate)
	})
}

func (td *TaskDir) candidateForTest(verdict, basename string, content []byte, predicate task.PredicateRef) (*task.OutcomeRecord, error, error) {
	_, _, meta, spec, metaHash, e := td.loadAndValidatePreparedSet()
	if e != nil {
		return nil, nil, e
	}
	if !predicate.Equal(meta.Predicate) {
		return nil, nil, task.ErrIncompatiblePredicate
	}
	candidate := task.OutcomeRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, SpecSHA256: spec, MetaSHA256: metaHash, Verdict: verdict, Predicate: predicate, Payload: task.PayloadDescriptor{Basename: basename, Length: int64(len(content)), SHA256: task.ComputeSHA256(content)}}
	// Validate the proposed descriptor before opening or staging any candidate.
	candidate.EvidenceSHA256 = task.ComputeSHA256(nil)
	if e = task.ValidateOutcomeRecord(&candidate); e != nil {
		return nil, nil, e
	}
	winner, e := td.readWinner(meta, spec, metaHash)
	if e == nil {
		candidate.EvidenceSHA256 = winner.EvidenceSHA256
		winner, e = td.acknowledgeWinner(winner)
		if e != nil {
			return winner, nil, e
		}
		if !task.CompareOutcomes(&candidate, winner) {
			return winner, nil, task.ErrOutcomeConflict
		}
		return winner, nil, nil
	}
	if !errors.Is(e, errNoOutcome) {
		return nil, nil, e
	}
	exists, e := td.store.exists(filepath.Join(td.Dir, "outcome.json"))
	if e != nil {
		return nil, nil, e
	}
	if exists {
		return nil, nil, task.ErrEvidenceFault
	}
	seal, e := td.readSeal(predicate, spec, metaHash)
	if e != nil {
		return nil, nil, e
	}
	candidate.EvidenceSHA256 = seal.ManifestSHA256
	return td.publishCandidate(&candidate, bytes.NewReader(content))
}

func (td *TaskDir) OpenPayload(outcome *task.OutcomeRecord) (io.ReadCloser, error) {
	if err := task.ValidateOutcomeRecord(outcome); err != nil {
		return nil, err
	}
	return td.store.openFile(filepath.Join(td.Dir, outcome.Payload.Basename), unix.O_RDONLY)
}

// PublishRecordCandidateForTest injects a complete rival descriptor solely for
// adversarial fixtures. A foreign candidate cannot authorize a new outcome.
func (td *TaskDir) PublishRecordCandidateForTest(candidate *task.OutcomeRecord, content []byte) (*task.OutcomeRecord, error, error) {
	return td.withCollection(func() (*task.OutcomeRecord, error, error) {
		if err := task.ValidateOutcomeRecord(candidate); err != nil {
			return nil, nil, err
		}
		_, _, meta, spec, metaHash, err := td.loadAndValidatePreparedSet()
		if err != nil {
			return nil, nil, err
		}
		winner, err := td.readWinner(meta, spec, metaHash)
		if err == nil {
			winner, err = td.acknowledgeWinner(winner)
			if err != nil {
				return winner, nil, err
			}
			if !task.CompareOutcomes(candidate, winner) {
				return winner, nil, task.ErrOutcomeConflict
			}
			return winner, nil, nil
		}
		if !errors.Is(err, errNoOutcome) {
			return nil, nil, err
		}
		return td.validateRecordCandidate(candidate, content, meta, spec, metaHash)
	})
}

func (td *TaskDir) validateRecordCandidate(candidate *task.OutcomeRecord, content []byte, meta *task.MetaRecord, spec, metaHash string) (*task.OutcomeRecord, error, error) {
	if candidate.RootID != td.store.RootID || candidate.TaskID != td.TaskID || candidate.SpecSHA256 != spec || candidate.MetaSHA256 != metaHash {
		return nil, nil, task.ErrIdentityMismatch
	}
	if !candidate.Predicate.Equal(meta.Predicate) {
		return nil, nil, task.ErrIncompatiblePredicate
	}
	if candidate.Payload.Length != int64(len(content)) || candidate.Payload.SHA256 != task.ComputeSHA256(content) {
		return nil, nil, task.ErrEvidenceFault
	}
	seal, err := td.readSeal(meta.Predicate, spec, metaHash)
	if err != nil {
		return nil, nil, err
	}
	if candidate.EvidenceSHA256 != seal.ManifestSHA256 {
		return nil, nil, task.ErrIdentityMismatch
	}
	exists, err := td.store.exists(filepath.Join(td.Dir, "outcome.json"))
	if err != nil {
		return nil, nil, err
	}
	if exists {
		return nil, nil, task.ErrEvidenceFault
	}
	return td.publishCandidate(candidate, bytes.NewReader(content))
}
