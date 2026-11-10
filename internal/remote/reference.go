package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Reference persists origin intent only. The destination owns all task state.
// The caller closes the reference before closing its taskdir.Store.
type Reference struct{ records *taskdir.ControlDir }

type SendIntent struct {
	IntentSHA256 string `json:"intent_sha256"`
}
type Acceptance struct {
	IntentSHA256 string   `json:"intent_sha256"`
	Identity     Identity `json:"identity"`
	TaskID       string   `json:"task_id"`
}
type Observation struct {
	Sequence   uint64   `json:"sequence,omitempty"`
	ObservedAt string   `json:"observed_at"`
	Response   Response `json:"response"`
}
type briefChunk struct {
	Index int    `json:"index"`
	Data  []byte `json:"data"`
}
type operationRecord struct {
	Operation Operation `json:"operation"`
	Control   Control   `json:"control"`
}

func OpenReference(ctx context.Context, store *taskdir.Store, id string, create bool) (*Reference, error) {
	records, err := store.OpenRemoteReferenceContext(ctx, id, create)
	if err != nil {
		return nil, err
	}
	return &Reference{records: records}, nil
}
func (r *Reference) Close() error { return r.records.Close() }

// IntentDigest uses task's canonical JSON contract, independent of framing.
func IntentDigest(intent Intent) (string, error) {
	if err := intent.Validate(); err != nil {
		return "", err
	}
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

// SaveIntent persists chunks first and the immutable intent last. An intent's
// existence therefore implies its exact brief bytes are available for recovery.
func (r *Reference) SaveIntent(ctx context.Context, intent Intent, brief []byte) error {
	var configErr error
	intent.Config, configErr = NormalizeOutputSilenceConfig(intent.Config)
	if configErr != nil {
		return configErr
	}

	if _, err := IntentDigest(intent); err != nil {
		return err
	}
	if int64(len(brief)) != intent.Brief.Length || task.ComputeSHA256(brief) != intent.Brief.SHA256 {
		return ErrConflict
	}
	return r.records.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var existing Intent
		err := tx.Read("intent.json", &existing)
		if err == nil {
			if verifyErr := tx.Verify("intent.json", intent); verifyErr != nil {
				return errors.Join(ErrConflict, verifyErr)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for offset, index := 0, 0; offset < len(brief); index++ {
			end := offset + MaxDataBytes
			if end > len(brief) {
				end = len(brief)
			}
			chunk := briefChunk{Index: index, Data: brief[offset:end]}
			if _, err = tx.Put(fmt.Sprintf("brief-%06d.json", index), chunk); err != nil {
				return err
			}
			offset = end
		}
		_, err = tx.Put("intent.json", intent)
		return err
	})
}

func (r *Reference) Intent() (Intent, error) {
	var intent Intent
	if err := r.records.Read("intent.json", &intent); err != nil {
		return intent, err
	}
	return intent, intent.Validate()
}

func (r *Reference) Brief() ([]byte, error) {
	intent, err := r.Intent()
	if err != nil {
		return nil, err
	}
	result := make([]byte, 0, int(intent.Brief.Length))
	for index := 0; int64(len(result)) < intent.Brief.Length; index++ {
		var chunk briefChunk
		if err = r.records.Read(fmt.Sprintf("brief-%06d.json", index), &chunk); err != nil {
			return nil, err
		}
		remaining := intent.Brief.Length - int64(len(result))
		if chunk.Index != index || len(chunk.Data) == 0 || len(chunk.Data) > MaxDataBytes || int64(len(chunk.Data)) > remaining {
			return nil, ErrConflict
		}
		result = append(result, chunk.Data...)
	}
	if task.ComputeSHA256(result) != intent.Brief.SHA256 {
		return nil, ErrConflict
	}
	return result, nil
}

// beginSend selects admit exactly once locally. Every later attempt reconciles.
func (r *Reference) beginSend(ctx context.Context, digest string) (bool, error) {
	first := false
	err := r.records.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var marker SendIntent
		err := tx.Read("send-intent.json", &marker)
		if err == nil {
			if marker.IntentSHA256 != digest {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		first, err = tx.Put("send-intent.json", SendIntent{IntentSHA256: digest})
		return err
	})
	return first, err
}

func (r *Reference) recordAcceptance(ctx context.Context, intent Intent, digest string, response Response) error {
	if !response.Existing || response.RequestSHA256 != digest {
		return ErrAcceptanceUnknown
	}
	_, err := r.records.PutContext(ctx, "accepted.json", Acceptance{IntentSHA256: digest, Identity: intent.Destination, TaskID: intent.TaskID})
	return err
}

func (r *Reference) Accepted() (Acceptance, error) {
	var accepted Acceptance
	if err := r.records.Read("accepted.json", &accepted); err != nil {
		return accepted, err
	}
	intent, err := r.Intent()
	if err != nil {
		return accepted, err
	}
	digest, err := IntentDigest(intent)
	if err != nil {
		return accepted, err
	}
	if accepted.TaskID != intent.TaskID || accepted.IntentSHA256 != digest {
		return accepted, ErrConflict
	}
	return accepted, intent.Destination.Match(accepted.Identity)
}

const retainedObservations = 2

func (r *Reference) Observe(ctx context.Context, response Response) error {
	if err := response.Validate(); err != nil {
		return err
	}
	return r.records.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		names, err := observationNames(tx)
		if err != nil {
			return err
		}
		if _, err = verifyRetainedObservations(tx, names); err != nil {
			return err
		}
		if err = pruneObservations(tx, names, retainedObservations); err != nil {
			return err
		}
		var sequence uint64
		if len(names) > 0 {
			sequence, _ = observationSequence(names[0])
		}
		if sequence == ^uint64(0) {
			return fmt.Errorf("%w: observation sequence exhausted", ErrProtocol)
		}
		sequence++
		name := fmt.Sprintf("observation-%020d.json", sequence)
		observation := Observation{Sequence: sequence, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Response: response}
		if _, err = tx.Put(name, observation); err != nil {
			return err
		}
		// A crash here leaves at most three records. The next writer first retires
		// the extra old generation before publishing another cache record.
		return pruneObservations(tx, append([]string{name}, names[:min(len(names), retainedObservations)]...), retainedObservations)
	})
}

func observationNames(tx *taskdir.ControlTransaction) ([]string, error) {
	names, err := tx.RecordNames()
	if err != nil {
		return nil, err
	}
	observations := make([]string, 0, retainedObservations+1)
	for _, name := range names {
		if _, ok := observationSequence(name); ok {
			observations = append(observations, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(observations)))
	return observations, nil
}

func readObservation(tx *taskdir.ControlTransaction, name string) (Observation, error) {
	var observation Observation
	if err := tx.Read(name, &observation); err != nil {
		return observation, err
	}
	return observation, validateObservation(name, observation)
}

func validateObservation(name string, observation Observation) error {
	sequence, ok := observationSequence(name)
	if !ok || sequence == 0 || observation.Sequence != sequence {
		return fmt.Errorf("%w: observation generation mismatch", ErrProtocol)
	}
	if _, err := time.Parse(time.RFC3339Nano, observation.ObservedAt); err != nil {
		return fmt.Errorf("%w: invalid observation timestamp", ErrProtocol)
	}
	if err := observation.Response.Validate(); err != nil {
		return err
	}
	return nil
}

func verifyRetainedObservations(tx *taskdir.ControlTransaction, names []string) (latest Observation, resultErr error) {
	for index, name := range names[:min(len(names), retainedObservations)] {
		observation, err := readObservation(tx, name)
		if err != nil {
			return latest, err
		}
		if err = tx.Verify(name, observation); err != nil {
			return latest, err
		}
		if index == 0 {
			latest = observation
		}
	}
	return latest, nil
}

func pruneObservations(tx *taskdir.ControlTransaction, names []string, keep int) error {
	for _, name := range names[min(len(names), keep):] {
		observation, err := readObservation(tx, name)
		if err != nil {
			return err
		}
		if err = tx.RemoveCacheRecord("observation", observation.Sequence, observation); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reference) SaveControl(ctx context.Context, operation Operation, control Control) error {
	if operation != Cancel && operation != Reply && operation != Steer {
		return fmt.Errorf("%w: not a control operation", ErrProtocol)
	}
	if err := task.ValidateTaskID(control.ID); err != nil {
		return err
	}
	if control.Message != nil && control.Message.ID != control.ID {
		return ErrConflict
	}
	_, err := r.records.PutContext(ctx, "control-"+control.ID+".json", operationRecord{Operation: operation, Control: control})
	if err != nil {
		return errors.Join(ErrConflict, err)
	}
	return nil
}

func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// LastObservation is a dated cache only. It does not authorize admission,
// recovery, cancellation, or publication; the remote record remains authority.
func (r *Reference) LastObservation(ctx context.Context) (latest Observation, resultErr error) {
	resultErr = r.records.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		names, err := observationNames(tx)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return os.ErrNotExist
		}
		latest, err = verifyRetainedObservations(tx, names)
		return err
	})
	return latest, resultErr
}

// ReadLastObservation validates the two retained origin observations without
// opening a writable reference, taking its control lock or pruning history.
func ReadLastObservation(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, intent Intent) (Observation, error) {
	if err := intent.Validate(); err != nil {
		return Observation{}, err
	}
	if intent.TaskID != id || intent.OriginRootID != reader.RootID() {
		return Observation{}, ErrIdentity
	}
	names, err := reader.RemoteIndexObservationNames(ctx, id)
	if err != nil {
		return Observation{}, err
	}
	if len(names) == 0 {
		return Observation{}, os.ErrNotExist
	}
	var latest Observation
	for index, name := range names {
		var observation Observation
		if err = reader.ReadCanonicalRemoteRecord(ctx, id, true, false, name, &observation); err != nil {
			return Observation{}, err
		}
		if err = validateObservation(name, observation); err != nil {
			return Observation{}, err
		}
		if observation.Response.Identity.RootID != "" {
			if err = intent.Destination.Match(observation.Response.Identity); err != nil {
				return Observation{}, err
			}
		}
		if index == 0 {
			latest = observation
		}
	}
	return latest, nil
}

func observationSequence(name string) (uint64, bool) {
	suffix, ok := strings.CutPrefix(name, "observation-")
	if !ok {
		return 0, false
	}
	suffix, ok = strings.CutSuffix(suffix, ".json")
	if !ok || len(suffix) != 20 {
		return 0, false
	}
	value, err := strconv.ParseUint(suffix, 10, 64)
	return value, err == nil
}

// sendRecorded is a read, not a submission gate. beginSend serializes the actual
// before-write boundary after a verified channel is available.
func (r *Reference) sendRecorded(digest string) (bool, error) {
	var marker SendIntent
	if err := r.records.Read("send-intent.json", &marker); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if marker.IntentSHA256 != digest {
		return false, ErrConflict
	}
	return true, nil
}

type admissionRejection struct {
	IntentSHA256 string `json:"intent_sha256"`
	Code         string `json:"code"`
	Message      string `json:"message"`
}

func (r *Reference) recordRejection(ctx context.Context, digest string, response Response) error {
	rejection := admissionRejection{IntentSHA256: digest, Code: "admission_rejected", Message: ErrAdmissionRejected.Error()}
	if response.Failure != nil {
		rejection.Code = response.Failure.Code
		rejection.Message = response.Failure.Message
	}
	_, err := r.records.PutContext(ctx, "rejected.json", rejection)
	return err
}

func (r *Reference) rejectedResponse(intent Intent, digest string) (Response, error) {
	var rejection admissionRejection
	if err := r.records.Read("rejected.json", &rejection); err != nil {
		return Response{}, err
	}
	if rejection.IntentSHA256 != digest {
		return Response{}, ErrConflict
	}
	return Response{Version: ProtocolVersion, Identity: intent.Destination, TaskRecord: "not_created", Failure: &Failure{Code: rejection.Code, Message: rejection.Message}}, ErrAdmissionRejected
}
