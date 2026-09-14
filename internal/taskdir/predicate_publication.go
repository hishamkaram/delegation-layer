package taskdir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type payloadStage struct {
	mu          sync.Mutex
	store       *Store
	file        *os.File
	stage, dest string
	digest      hash.Hash
	size        int64
	accepting   bool
	closed      bool
	failure     error
}

func (td *TaskDir) newPayloadStage(name string) (*payloadStage, error) {
	id, err := task.NewRandomID()
	if err != nil {
		return nil, err
	}
	stage := filepath.Join(td.Dir, "stage."+id+".tmp")
	dest := filepath.Join(td.Dir, name)
	if observer, ok := td.store.faultInjector.(StageCreateInjector); ok {
		if err = observer.OnBeforeStageCreate(stage, dest); err != nil {
			return nil, err
		}
	}
	f, err := td.store.openFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return nil, err
	}
	return &payloadStage{store: td.store, file: f, stage: stage, dest: dest, digest: sha256.New(), accepting: true}, nil
}

func (s *payloadStage) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.accepting {
		return 0, os.ErrClosed
	}
	if s.failure != nil {
		return 0, s.failure
	}
	if err := writeStageContent(s.file, p, s.dest, s.store.faultInjector); err != nil {
		s.failure = err
		return 0, err
	}
	n, err := s.digest.Write(p)
	s.size += int64(n)
	s.failure = err
	return n, err
}

func (s *payloadStage) expire() {
	s.mu.Lock()
	s.accepting = false
	s.mu.Unlock()
}

func (s *payloadStage) finish() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.failure
	}
	s.closed = true
	s.accepting = false
	if s.failure == nil {
		s.failure = barrierAndCloseStage(s.file, s.dest, s.store.faultInjector)
		if s.failure == nil {
			return nil
		}
	}
	s.failure = errors.Join(s.failure, s.file.Close())
	return s.failure
}

func (s *payloadStage) discard() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var closeErr error
	if !s.closed {
		s.closed = true
		s.accepting = false
		closeErr = s.file.Close()
	}
	return errors.Join(closeErr, s.store.cleanupStage(s.stage, s.dest, s.store.faultInjector))
}

func (s *payloadStage) descriptor() task.PayloadDescriptor {
	return task.PayloadDescriptor{Basename: filepath.Base(s.dest), Length: s.size, SHA256: hex.EncodeToString(s.digest.Sum(nil))}
}

func (td *TaskDir) interpretationInput(seal *task.ProviderExitRecord) (predicate.Input, error) {
	_, req, _, _, _, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return predicate.Input{}, err
	}
	input := predicate.Input{Seal: *seal}
	input.Seal.RawManifest = append([]task.RawManifestEntry(nil), seal.RawManifest...)
	if req.PriorSession != nil {
		input.ExpectedSession = task.SessionExpectation{Required: true, ID: req.PriorSession.ConversationID}
	}
	recorded, err := td.ReadProviderIdentity()
	if err == nil {
		input.RecordedSession = &task.SessionIdentity{Provider: recorded.Provider, ConversationID: recorded.ConversationID}
	} else if !errors.Is(err, os.ErrNotExist) {
		return predicate.Input{}, err
	}
	return input, nil
}

func (td *TaskDir) evaluateAndPublish(interpreter predicate.Interpreter, seal *task.ProviderExitRecord, spec, meta string) (*task.OutcomeRecord, error, error) {
	evidence, err := td.openValidatedEvidence(seal)
	if err != nil {
		return nil, nil, err
	}
	input, err := td.interpretationInput(seal)
	if err != nil {
		return nil, nil, errors.Join(err, evidence.Close())
	}
	stage, err := td.newPayloadStage("result.txt")
	if err != nil {
		return nil, nil, errors.Join(err, evidence.Close())
	}
	interpretation, evalErr := interpreter.Evaluate(input, evidence, stage)
	// The interpreter's capability ends at return. Expire writes before any
	// evidence cleanup or validation so a retained writer cannot append after
	// evaluation has made its decision.
	stage.expire()
	err = errors.Join(evalErr, evidence.Close(), stage.writeFailure(), task.ValidateInterpretation(interpretation), validateInterpretedIdentity(input, interpretation))
	if err != nil {
		return nil, stage.discard(), err
	}
	if interpretation.Verdict == task.VerdictRejected {
		if err = stage.discard(); err != nil {
			return nil, nil, err
		}
		stage, err = td.newPayloadStage("publish.reject")
		if err != nil {
			return nil, nil, err
		}
		if _, err = io.WriteString(stage, interpretation.Refusal); err != nil {
			return nil, stage.discard(), err
		}
	}
	if err = stage.finish(); err != nil {
		return nil, stage.discard(), err
	}
	descriptor := stage.descriptor()
	candidate := task.OutcomeRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, SpecSHA256: spec, MetaSHA256: meta, Predicate: seal.Predicate, EvidenceSHA256: seal.ManifestSHA256, Verdict: interpretation.Verdict, Payload: descriptor}
	if err = task.ValidateOutcomeRecord(&candidate); err != nil {
		return nil, stage.discard(), err
	}
	data, err := marshalControlRecord(&candidate)
	if err != nil {
		return nil, stage.discard(), err
	}
	cleanup, err := td.commitPayloadStage(stage)
	if err != nil || cleanup != nil {
		return nil, cleanup, err
	}
	outcome, outCleanup, err := td.commitOutcome(&candidate, data)
	return outcome, errors.Join(cleanup, outCleanup), err
}

func (td *TaskDir) commitPayloadStage(stage *payloadStage) (error, error) {
	_, cleanup, err := td.store.commitStage(stage.stage, stage.dest, td.store.faultInjector)
	if errors.Is(err, os.ErrExist) {
		cleanup = errors.Join(cleanup, stage.discard())
		err = nil
	}
	if err != nil {
		return cleanup, err
	}
	digest, size, err := td.store.fileDigest(stage.dest)
	if err != nil {
		return cleanup, errors.Join(task.ErrInvariantFault, err)
	}
	if digest != stage.descriptor().SHA256 || size != stage.size {
		return cleanup, task.ErrInvariantFault
	}
	return cleanup, td.store.barrierDir(td.Dir)
}

func (s *payloadStage) writeFailure() error { s.mu.Lock(); defer s.mu.Unlock(); return s.failure }
func validateInterpretedIdentity(input predicate.Input, result task.Interpretation) error {
	if result.Session == nil {
		return nil
	}
	session := result.Session
	if session.Provider != input.Seal.Predicate.Adapter {
		return task.ErrIdentityMismatch
	}
	if input.ExpectedSession.Required && session.ConversationID != input.ExpectedSession.ID {
		return task.ErrIdentityMismatch
	}
	if input.RecordedSession != nil && *session != *input.RecordedSession {
		return task.ErrIdentityMismatch
	}
	return nil
}
