package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func (c *Catalog) readMetadata(ctx context.Context, id Identity) (Row, error) {
	value := Row{Identity: id}
	if id.Kind == taskdir.IndexLocal {
		metadata, err := c.reader.ReadTaskIndexMetadata(ctx, id.TaskID)
		value.Metadata = metadata
		return value, err
	}
	intent, err := readIntent(ctx, c.reader, id.TaskID)
	if err != nil {
		return value, err
	}
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		return value, err
	}
	value.Metadata = taskdir.IndexMetadata{
		TaskID: id.TaskID, Provider: intent.Provider, Model: intent.Config.Model,
		Effort: intent.Config.Effort, Cwd: intent.RemoteCwd, RequestDigest: digest,
	}
	value.Device = intent.Device
	return value, nil
}

func readIntent(ctx context.Context, reader *taskdir.ReadOnlyStore, id string) (remote.Intent, error) {
	var intent remote.Intent
	if err := reader.ReadRemoteRecord(ctx, id, true, false, "intent.json", &intent); err != nil {
		return intent, err
	}
	if err := intent.Validate(); err != nil {
		return intent, err
	}
	if intent.TaskID != id || intent.OriginRootID != reader.RootID() {
		return intent, task.ErrIdentityMismatch
	}
	return intent, nil
}

// Observe reads fresh passive source facts for a visible-row overlay. Secondary
// sessions never persist these observations; lifecycle remains authoritative.
func (c *Catalog) Observe(ctx context.Context, id Identity) (Observation, error) {
	results, err := c.ObserveBatch(ctx, []Identity{id})
	if err != nil {
		return Observation{}, err
	}
	return results[0].Observation, results[0].Err
}

// ObserveBatch returns one result per identity in input order. Root identity is
// checked around the bounded refresh, not reopened once per visible row. This
// is a dated observation, not an atomic snapshot: replacement during the read
// rejects the batch, while entry faults preserve the caller's previous facts.
func (c *Catalog) ObserveBatch(ctx context.Context, ids []Identity) ([]ObservationResult, error) {
	c.use.RLock()
	defer c.use.RUnlock()
	if len(ids) > maximumPageSize {
		return nil, errors.New("visible observation batch cannot exceed 200 identities")
	}
	for _, id := range ids {
		if err := id.Validate(); err != nil {
			return nil, err
		}
	}
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	results := make([]ObservationResult, len(ids))
	for index, id := range ids {
		observed, err := observeSource(ctx, c.viewer, id)
		if errors.Is(err, ErrRootChanged) {
			return nil, err
		}
		if cancelErr := ctx.Err(); cancelErr != nil {
			return nil, cancelErr
		}
		results[index] = ObservationResult{Observation: observed, Err: err}
	}
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	return results, nil
}

func observeSource(ctx context.Context, reader *taskdir.ReadOnlyStore, id Identity) (Observation, error) {
	if id.Kind == taskdir.IndexRemote {
		return observeRemote(ctx, reader, id.TaskID)
	}
	facts, observed, err := tasklifecycle.ReadPassiveTaskObservation(ctx, reader, id.TaskID)
	if err != nil {
		return Observation{}, err
	}
	if !facts.Exists {
		return Observation{}, os.ErrNotExist
	}
	return Observation{
		At: time.Now().UTC(), Admission: observed.Admission, Liveness: observed.Liveness,
		Publication: observed.Publication, Status: observed.Status, FailureCode: observed.FailureCode, Source: "passive",
	}, nil
}

func observeRemote(ctx context.Context, reader *taskdir.ReadOnlyStore, id string) (Observation, error) {
	intent, err := readIntent(ctx, reader, id)
	if err != nil {
		return Observation{}, err
	}
	value, err := remote.ReadLastObservation(ctx, reader, id, intent)
	if err != nil {
		return Observation{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, value.ObservedAt)
	if err != nil {
		return Observation{}, err
	}
	var envelope struct {
		TaskID      string `json:"task_id"`
		RootID      string `json:"root_id"`
		Admission   string `json:"admission"`
		Liveness    string `json:"liveness"`
		Publication string `json:"publication"`
		Status      string `json:"status"`
		Failure     *struct {
			Code string `json:"code"`
		} `json:"failure"`
	}
	if err = json.Unmarshal(value.Response.Envelope, &envelope); err != nil {
		return Observation{}, err
	}
	if envelope.TaskID != intent.TaskID || envelope.RootID != intent.Destination.RootID {
		return Observation{}, task.ErrIdentityMismatch
	}
	observation := Observation{At: at, Status: envelope.Status, Source: "origin-cache"}
	if envelope.Failure != nil {
		observation.FailureCode = envelope.Failure.Code
	}
	if err = parseObservation(&observation, envelope.Admission, envelope.Liveness, envelope.Publication); err != nil {
		return Observation{}, err
	}
	return observation, nil
}

func parseObservation(value *Observation, admission, liveness, publication string) error {
	admissions := map[string]task.Admission{"unknown": task.AdmissionUnknown, "not-admitted": task.AdmissionNotAdmitted, "admitted": task.AdmissionAdmitted}
	livenesses := map[string]task.Liveness{"undetermined": task.LivenessUndetermined, "running": task.LivenessRunning, "ended": task.LivenessEnded}
	publications := map[string]task.Publication{"unknown": task.PublicationUnknown, "pending": task.PublicationPending, "committed": task.PublicationCommitted, "rejected": task.PublicationRejected}
	var ok bool
	if value.Admission, ok = admissions[admission]; !ok {
		return fmt.Errorf("%w: invalid cached admission", remote.ErrProtocol)
	}
	if value.Liveness, ok = livenesses[liveness]; !ok {
		return fmt.Errorf("%w: invalid cached liveness", remote.ErrProtocol)
	}
	if value.Publication, ok = publications[publication]; !ok {
		return fmt.Errorf("%w: invalid cached publication", remote.ErrProtocol)
	}
	if len(value.Status) > 128 || len(value.FailureCode) > 128 {
		return errors.New("cached observation fields exceed bounds")
	}
	return nil
}
