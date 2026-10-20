package contract

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	TurnObserved    = "observed"
	TurnUnavailable = "unavailable"
	TurnChanged     = "changed"
	TurnUnchanged   = "unchanged"

	// TurnBaselineUnavailable marks a turn without a prelaunch baseline, such
	// as one admitted before baselines were recorded.
	TurnBaselineUnavailable = "baseline_unavailable"
)

// Capture artifact names. The cumulative pair diffs the workspace base to the
// final capture tree. The turn pair exists only for an observed turn and diffs
// that turn's prelaunch baseline tree to the same final tree, so it excludes
// earlier turns and changes made before launch.
const (
	CapturePatchName      = "diff.patch"
	CaptureNameStatusName = "name-status.z"
	TurnPatchName         = "turn-diff.patch"
	TurnNameStatusName    = "turn-name-status.z"
)

// CaptureArtifactNames returns the cumulative and optional turn artifact names.
func CaptureArtifactNames() (cumulative, turn []string) {
	return []string{CapturePatchName, CaptureNameStatusName}, []string{TurnPatchName, TurnNameStatusName}
}

// ObservedTurn reports whether the candidate compares against a recorded
// prelaunch baseline.
func (c CaptureCandidate) ObservedTurn() bool {
	return c.Turn != nil && c.Turn.Status == TurnObserved
}

// ValidateTurnArtifacts requires the complete turn artifact pair exactly when
// the captured candidate observed its turn. An unavailable turn has no turn
// files, never an empty or clean inventory.
func ValidateTurnArtifacts(candidate CaptureCandidate, artifacts []Artifact) error {
	present := map[string]bool{}
	for _, artifact := range artifacts {
		if artifact.Name != TurnPatchName && artifact.Name != TurnNameStatusName {
			continue
		}
		if present[artifact.Name] {
			return task.ErrIdentityMismatch
		}
		present[artifact.Name] = true
	}
	if present[TurnPatchName] != candidate.ObservedTurn() || present[TurnNameStatusName] != candidate.ObservedTurn() {
		return task.ErrIdentityMismatch
	}
	return nil
}

// TurnBaseline is the content observed under an accepted task reservation
// immediately before provider launch. It is create-once evidence, never
// inferred from a predecessor's capture, and Ref keeps its tree reachable.
type TurnBaseline struct {
	SchemaVersion int     `json:"schema_version"`
	WorkspaceID   string  `json:"workspace_id"`
	Sequence      uint64  `json:"sequence"`
	Subject       Subject `json:"subject"`
	BaseOID       string  `json:"base_oid"`
	ObservedHEAD  string  `json:"observed_head"`
	TreeOID       string  `json:"tree_oid"`
	IndexSHA256   string  `json:"index_sha256"`
	ObjectFormat  string  `json:"object_format"`
	Ref           string  `json:"ref"`
}

// Observation returns the baseline in the capture-candidate shape consumed by
// OID format and source-base validation.
func (baseline TurnBaseline) Observation() CaptureCandidate {
	return CaptureCandidate{BaseOID: baseline.BaseOID, ObservedHEAD: baseline.ObservedHEAD, TreeOID: baseline.TreeOID, ObjectFormat: baseline.ObjectFormat}
}

// TurnObservation compares one provider turn's baseline with its final
// capture. It reports net observed tree, HEAD and real-index differences;
// writes reverted before capture, ignored files and transient state are not
// observable. Unavailable carries no change verdict. It is workspace evidence,
// not a sandbox or provider outcome.
type TurnObservation struct {
	Status           string `json:"status"`
	ReasonCode       string `json:"reason_code,omitempty"`
	BaselineRef      string `json:"baseline_ref,omitempty"`
	StartHEAD        string `json:"start_head,omitempty"`
	StartTreeOID     string `json:"start_tree_oid,omitempty"`
	StartIndexSHA256 string `json:"start_index_sha256,omitempty"`
	Content          string `json:"content,omitempty"`
	HEAD             string `json:"head,omitempty"`
	Index            string `json:"index,omitempty"`
}

// ObserveTurn derives the turn comparison from immutable observations only.
// A nil baseline is unavailable rather than clean.
func ObserveTurn(baseline *TurnBaseline, final CaptureCandidate) *TurnObservation {
	if baseline == nil {
		return &TurnObservation{Status: TurnUnavailable, ReasonCode: TurnBaselineUnavailable}
	}
	observation := &TurnObservation{
		Status: TurnObserved, BaselineRef: baseline.Ref, StartHEAD: baseline.ObservedHEAD,
		StartTreeOID: baseline.TreeOID, StartIndexSHA256: baseline.IndexSHA256,
		Content: turnChange(baseline.TreeOID, final.TreeOID), HEAD: turnChange(baseline.ObservedHEAD, final.ObservedHEAD),
	}
	if final.IndexSHA256 != "" {
		observation.Index = turnChange(baseline.IndexSHA256, final.IndexSHA256)
	}
	return observation
}

func turnChange(start, final string) string {
	if start == final {
		return TurnUnchanged
	}
	return TurnChanged
}

func ValidateTurnBaseline(baseline TurnBaseline, workspaceID string, sequence uint64) error {
	if baseline.SchemaVersion != task.SchemaVersion || baseline.WorkspaceID != workspaceID || sequence == 0 || baseline.Sequence != sequence {
		return task.ErrIdentityMismatch
	}
	if baseline.Subject.Kind != SubjectTask {
		return task.ErrIdentityMismatch
	}
	if err := errors.Join(task.ValidateTaskID(baseline.WorkspaceID), ValidateSubject(baseline.Subject), task.ValidateSHA256(baseline.IndexSHA256), ValidateOwnedRef(baseline.Ref)); err != nil {
		return err
	}
	for _, oid := range []string{baseline.BaseOID, baseline.ObservedHEAD, baseline.TreeOID} {
		if err := ValidateOID(oid, baseline.ObjectFormat); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTurnObservation accepts an unavailable observation without
// verdicts or a complete observed comparison.
func ValidateTurnObservation(turn TurnObservation, objectFormat string) error {
	switch turn.Status {
	case TurnUnavailable:
		if turn != (TurnObservation{Status: TurnUnavailable, ReasonCode: TurnBaselineUnavailable}) {
			return task.ErrIdentityMismatch
		}
		return nil
	case TurnObserved:
		return validateObservedTurn(turn, objectFormat)
	default:
		return task.ErrIdentityMismatch
	}
}

func validateObservedTurn(turn TurnObservation, objectFormat string) error {
	if turn.ReasonCode != "" || !validTurnChange(turn.Content) || !validTurnChange(turn.HEAD) || (turn.Index != "" && !validTurnChange(turn.Index)) {
		return task.ErrIdentityMismatch
	}
	return errors.Join(ValidateOwnedRef(turn.BaselineRef), ValidateOID(turn.StartHEAD, objectFormat),
		ValidateOID(turn.StartTreeOID, objectFormat), task.ValidateSHA256(turn.StartIndexSHA256))
}

func validTurnChange(value string) bool {
	return value == TurnChanged || value == TurnUnchanged
}
