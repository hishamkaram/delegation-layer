package taskdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type TaskInspection struct {
	TaskID                 string              `json:"task_id"`
	BriefExists            bool                `json:"brief_exists"`
	TaskExists             bool                `json:"task_exists"`
	MetaExists             bool                `json:"meta_exists"`
	SubmitExists           bool                `json:"submit_exists"`
	StartExists            bool                `json:"start_exists"`
	StartedExists          bool                `json:"started_exists"`
	SealExists             bool                `json:"seal_exists"`
	OutcomeExists          bool                `json:"outcome_exists"`
	PayloadBasename        string              `json:"payload_basename,omitempty"`
	ExtraPayloadDiagnostic string              `json:"extra_payload_diagnostic,omitempty"`
	Publication            task.Publication    `json:"publication"`
	Outcome                *task.OutcomeRecord `json:"outcome,omitempty"`
}

func (td *TaskDir) Inspect() (*TaskInspection, error) {
	insp := &TaskInspection{TaskID: td.TaskID, Publication: task.PublicationUnknown}
	if err := td.inspectPresence(insp); err != nil {
		return insp, err
	}
	_, _, meta, spec, metaHash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return insp, err
	}
	if insp.OutcomeExists {
		return td.inspectWinner(insp, meta, spec, metaHash)
	}
	if insp.SealExists {
		if _, err = td.readSeal(meta.Predicate, spec, metaHash); err != nil {
			return insp, err
		}
		insp.Publication = task.PublicationPending
	}
	// An unselected payload alone provides no terminal authority.
	for _, name := range []string{"result.txt", "publish.reject"} {
		exists, e := td.store.exists(filepath.Join(td.Dir, name))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return insp, e
		}
		if exists {
			insp.PayloadBasename = name
			insp.Publication = task.PublicationPending
		}
	}
	return insp, nil
}

func (td *TaskDir) inspectPresence(insp *TaskInspection) error {
	for _, entry := range []struct {
		name    string
		present *bool
	}{
		{"brief.md", &insp.BriefExists}, {"task.json", &insp.TaskExists}, {"meta.json", &insp.MetaExists}, {"submit.json", &insp.SubmitExists}, {"provider.start", &insp.StartExists}, {"provider.started.json", &insp.StartedExists}, {"provider.exit", &insp.SealExists}, {"outcome.json", &insp.OutcomeExists},
	} {
		present, err := td.store.entryExists(filepath.Join(td.Dir, entry.name))
		*entry.present = present
		if err != nil {
			return err
		}
	}
	return nil
}

func (td *TaskDir) inspectWinner(insp *TaskInspection, meta *task.MetaRecord, spec, metaHash string) (*TaskInspection, error) {
	winner, e := td.readWinner(meta, spec, metaHash)
	if e != nil {
		return insp, e
	}
	insp.Outcome = winner
	insp.PayloadBasename = winner.Payload.Basename
	if winner.Verdict == task.VerdictCommitted {
		insp.Publication = task.PublicationCommitted
	} else {
		insp.Publication = task.PublicationRejected
	}
	other := "result.txt"
	if winner.Payload.Basename == other {
		other = "publish.reject"
	}
	extra, e := td.store.exists(filepath.Join(td.Dir, other))
	if e != nil {
		return insp, e
	}
	if extra {
		insp.ExtraPayloadDiagnostic = fmt.Sprintf("extra payload %s beside winner %s", other, winner.Payload.Basename)
	}
	return insp, nil
}
