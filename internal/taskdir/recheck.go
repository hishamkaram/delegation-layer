package taskdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	recheckReportName = "report.json"
	recheckResultName = "result.txt"
)

// RecheckResult describes a durable derived assessment. Paths are relative to
// the task directory so public callers never need to reconstruct state-root
// internals or receive provider output in a control response.
type RecheckResult struct {
	AssessmentID string
	Report       *task.RecheckReport
	ReportPath   string
	AnswerPath   string
}

type recheckSource struct {
	outcome             *task.OutcomeRecord
	seal                *task.ProviderExitRecord
	sourceOutcomeSHA256 string
	sourceSealSHA256    string
	target              task.PredicateRef
	assessmentID        string
	recheckDir          string
	relativeReport      string
	relativeAnswer      string
}

// Recheck verifies the original terminal evidence, then performs one pure
// offline recovery assessment. It never touches submission, supervisor, or
// provider execution records.
func (td *TaskDir) Recheck(interpreter predicate.RecoveryInterpreter) (result *RecheckResult, resultErr error) {
	if interpreter == nil {
		return nil, predicate.ErrRecoveryUnsupported
	}
	var recheckErr error
	resultErr = td.withRecheckLock(func() error {
		result, recheckErr = td.recheckOwned(interpreter)
		return recheckErr
	})
	return result, resultErr
}

// RecheckAvailable reports whether this source has an eligible target that has
// not already produced a validated derived report. It performs no evaluation
// and is used only to decorate collect's actionable failure envelope.
func (td *TaskDir) RecheckAvailable(interpreter predicate.RecoveryInterpreter) (available bool, resultErr error) {
	if interpreter == nil {
		return false, predicate.ErrRecoveryUnsupported
	}
	resultErr = td.withRecheckLock(func() error {
		source, err := td.loadRecheckSource(interpreter)
		if err != nil {
			return err
		}
		reportExists, err := td.store.entryExists(filepath.Join(source.recheckDir, recheckReportName))
		if errors.Is(err, os.ErrNotExist) {
			available = true
			return nil
		}
		if err != nil {
			return err
		}
		if !reportExists {
			available = true
			return nil
		}
		_, err = td.readCachedRecheck(source)
		return err
	})
	return available, resultErr
}

func (td *TaskDir) withRecheckLock(fn func() error) (resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := td.store.maintLock.LockSH(ctx); err != nil {
		return err
	}
	if err := td.runLock.LockEX(ctx); err != nil {
		return errors.Join(err, td.store.maintLock.Unlock())
	}
	defer func() { resultErr = errors.Join(resultErr, td.runLock.Unlock(), td.store.maintLock.Unlock()) }()
	return fn()
}

func (td *TaskDir) recheckOwned(interpreter predicate.RecoveryInterpreter) (*RecheckResult, error) {
	source, err := td.loadRecheckSource(interpreter)
	if err != nil {
		return nil, err
	}

	if exists, existsErr := td.store.entryExists(filepath.Join(source.recheckDir, recheckReportName)); existsErr == nil && exists {
		return td.readCachedRecheck(source)
	} else if existsErr != nil && !errors.Is(existsErr, os.ErrNotExist) {
		return nil, existsErr
	}

	if err = td.store.mkdir(source.recheckDir, td.store.faultInjector); err != nil {
		return nil, err
	}
	input, err := td.interpretationInput(source.seal)
	if err != nil {
		return nil, err
	}
	evidence, err := td.openValidatedEvidence(source.seal)
	if err != nil {
		return nil, err
	}
	stage, err := td.newPayloadStageAt(source.recheckDir, recheckResultName)
	if err != nil {
		return nil, errors.Join(err, evidence.Close())
	}
	interpretation, recoverErr := interpreter.Recover(input, evidence, stage)
	stage.expire()
	validationErr := errors.Join(
		recoverErr,
		evidence.Close(),
		stage.writeFailure(),
		task.ValidateInterpretation(interpretation),
		validateInterpretedIdentity(input, interpretation),
	)
	if validationErr != nil {
		return nil, errors.Join(validationErr, stage.discard())
	}

	report := task.RecheckReport{
		SchemaVersion:        task.RecheckSchemaVersion,
		AssessmentID:         source.assessmentID,
		SourceOutcomeSHA256:  source.sourceOutcomeSHA256,
		SourceSealSHA256:     source.sourceSealSHA256,
		SourcePredicate:      source.outcome.Predicate,
		SourceEvidenceSHA256: source.outcome.EvidenceSHA256,
		TargetPredicate:      source.target,
		Diagnostics:          task.CloneOutputDiagnostics(interpretation.Diagnostics),
	}
	if err = td.finishRecheckPayload(stage, source.recheckDir, interpretation.Verdict, &report); err != nil {
		return nil, err
	}
	if err = task.ValidateRecheckReport(&report); err != nil {
		return nil, err
	}
	return td.commitRecheckReport(source, &report)
}

func (td *TaskDir) finishRecheckPayload(stage *payloadStage, directory, verdict string, report *task.RecheckReport) error {
	var err error
	if verdict == task.VerdictCommitted {
		if stage.size <= 0 {
			return errors.Join(task.ErrInvariantFault, stage.discard())
		}
		if err = stage.finish(); err != nil {
			return errors.Join(err, stage.discard())
		}
		descriptor := stage.descriptor()
		if err = task.ValidatePayloadDescriptor(descriptor); err != nil {
			return errors.Join(err, stage.discard())
		}
		cleanupErr, commitErr := td.commitPayloadStage(stage)
		if cleanupErr != nil || commitErr != nil {
			return errors.Join(cleanupErr, commitErr)
		}
		report.Status = task.RecheckStatusRecovered
		report.Result = &descriptor
	} else {
		if err = stage.discard(); err != nil {
			return err
		}
		resultExists, existsErr := td.store.entryExists(filepath.Join(directory, recheckResultName))
		if existsErr != nil && !errors.Is(existsErr, os.ErrNotExist) {
			return existsErr
		}
		if resultExists {
			return task.ErrRecheckConflict
		}
		report.Status = task.RecheckStatusRejected
	}
	return nil
}

func (td *TaskDir) loadRecheckSource(interpreter predicate.RecoveryInterpreter) (*recheckSource, error) {
	_, _, meta, specHash, metaHash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	seal, err := td.readSeal(meta.Predicate, specHash, metaHash)
	if err != nil {
		return nil, err
	}
	// readWinner verifies the original payload and rehashes the full raw manifest,
	// including when an existing derived assessment is reused.
	outcome, err := td.readWinner(meta, specHash, metaHash)
	if err != nil {
		return nil, err
	}
	if outcome.Verdict != task.VerdictRejected {
		return nil, task.ErrRecheckNotRejected
	}
	if !outcome.Predicate.Equal(seal.Predicate) || outcome.EvidenceSHA256 != seal.ManifestSHA256 {
		return nil, task.ErrEvidenceFault
	}

	sourceOutcomeSHA256, _, err := td.store.fileDigest(filepath.Join(td.Dir, "outcome.json"))
	if err != nil {
		return nil, err
	}
	sourceSealSHA256, _, err := td.store.fileDigest(filepath.Join(td.Dir, "provider.exit"))
	if err != nil {
		return nil, err
	}
	target, err := recoveryTarget(interpreter, outcome.Predicate)
	if err != nil {
		return nil, err
	}
	assessmentID, err := task.RecheckAssessmentID(sourceOutcomeSHA256, sourceSealSHA256, target)
	if err != nil {
		return nil, err
	}
	relativeDir := filepath.Join("rechecks", assessmentID)
	return &recheckSource{
		outcome:             outcome,
		seal:                seal,
		sourceOutcomeSHA256: sourceOutcomeSHA256,
		sourceSealSHA256:    sourceSealSHA256,
		target:              target,
		assessmentID:        assessmentID,
		recheckDir:          filepath.Join(td.Dir, relativeDir),
		relativeReport:      filepath.Join(relativeDir, recheckReportName),
		relativeAnswer:      filepath.Join(relativeDir, recheckResultName),
	}, nil
}

func recoveryTarget(interpreter predicate.RecoveryInterpreter, source task.PredicateRef) (task.PredicateRef, error) {
	target := interpreter.Target()
	if err := task.ValidatePredicateRef(target); err != nil {
		return task.PredicateRef{}, fmt.Errorf("invalid recovery target: %w", err)
	}
	if !target.Equal(interpreter.Reference()) || source.Equal(target) || !interpreter.CanRecover(source) {
		return task.PredicateRef{}, predicate.ErrRecoveryUnsupported
	}
	return target, nil
}

func (td *TaskDir) readCachedRecheck(source *recheckSource) (*RecheckResult, error) {
	path := filepath.Join(source.recheckDir, recheckReportName)
	data, err := td.store.readBytes(path, task.MaxControlRecordSize)
	if err != nil {
		return nil, err
	}
	var report task.RecheckReport
	if err = task.DecodeStrict(data, &report); err != nil {
		return nil, errors.Join(task.ErrEvidenceFault, err)
	}
	canonical, err := task.MarshalCanonical(&report)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, canonical) {
		return nil, task.ErrEvidenceFault
	}
	if err = task.ValidateRecheckReport(&report); err != nil {
		return nil, err
	}
	if report.AssessmentID != source.assessmentID || report.SourceOutcomeSHA256 != source.sourceOutcomeSHA256 || report.SourceSealSHA256 != source.sourceSealSHA256 || !report.SourcePredicate.Equal(source.outcome.Predicate) || !report.SourcePredicate.Equal(source.seal.Predicate) || report.SourceEvidenceSHA256 != source.outcome.EvidenceSHA256 || !report.TargetPredicate.Equal(source.target) {
		return nil, task.ErrRecheckConflict
	}
	if err = td.validateRecheckPayload(source.recheckDir, &report); err != nil {
		return nil, err
	}
	return recheckResult(&report, source.relativeReport, source.relativeAnswer), td.acknowledgeRecord(source.relativeReport)
}

func (td *TaskDir) validateRecheckPayload(directory string, report *task.RecheckReport) error {
	resultPath := filepath.Join(directory, recheckResultName)
	resultExists, err := td.store.entryExists(resultPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if report.Result != nil {
		if !resultExists {
			return task.ErrEvidenceFault
		}
		digest, size, digestErr := td.store.fileDigest(resultPath)
		if digestErr != nil {
			return digestErr
		}
		if digest != report.Result.SHA256 || size != report.Result.Length {
			return task.ErrEvidenceFault
		}
	} else if resultExists {
		return task.ErrEvidenceFault
	}
	return nil
}

func (td *TaskDir) commitRecheckReport(source *recheckSource, report *task.RecheckReport) (*RecheckResult, error) {
	data, err := task.MarshalCanonical(report)
	if err != nil {
		return nil, err
	}
	committed, cleanupErr, commitErr := td.store.stageAndCommit(source.recheckDir, recheckReportName, data, td.store.faultInjector)
	if errors.Is(commitErr, os.ErrExist) && !committed {
		cached, readErr := td.readCachedRecheck(source)
		if readErr != nil {
			return cached, errors.Join(cleanupErr, readErr)
		}
		winnerData, marshalErr := task.MarshalCanonical(cached.Report)
		if marshalErr != nil {
			return cached, errors.Join(cleanupErr, marshalErr)
		}
		if !bytes.Equal(data, winnerData) {
			return cached, errors.Join(cleanupErr, task.ErrRecheckConflict)
		}
		return cached, cleanupErr
	}
	if commitErr != nil || cleanupErr != nil {
		return nil, errors.Join(cleanupErr, commitErr)
	}
	if !committed {
		return nil, task.ErrUncertainDurability
	}
	return recheckResult(report, source.relativeReport, source.relativeAnswer), nil
}

func recheckResult(report *task.RecheckReport, reportPath, answerPath string) *RecheckResult {
	result := &RecheckResult{AssessmentID: report.AssessmentID, Report: report, ReportPath: reportPath}
	if report.Result != nil {
		result.AnswerPath = answerPath
	}
	return result
}
