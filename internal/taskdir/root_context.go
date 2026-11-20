package taskdir

// Root context reconciliation stays with Store's retained root and maintenance
// authority. Task owns pure data validity; the caller owns runtime quiescence.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrRootContextNotRequired = errors.New("saved root context already matches without reconciliation")

// RootContextReconciliation contains only digests of token and history evidence.
type RootContextReconciliation struct {
	RootID   string                      `json:"root_id"`
	Origin   task.RootContextCoordinates `json:"origin"`
	Accepted task.RootContextCoordinates `json:"accepted"`
	// Internal challenge input. This is also a Gateway wire guard, not telemetry.
	TokenSHA256        string `json:"-"`
	RootRecordSHA256   string `json:"root_record_sha256"`
	HistorySHA256      string `json:"history_sha256"`
	Challenge          string `json:"challenge"`
	AlreadyReconciled  bool   `json:"already_reconciled"`
	TaskRequests       int    `json:"task_requests"`
	InspectionRequests int    `json:"inspection_requests"`
}

// RootContextReconciliationGate belongs to this consumer. The application must
// exclude runtime admission and all pending lifecycle transitions throughout fn.
// It synchronously calls fn once and joins its exclusions before returning.
// Its bootstrap exclusion is acquired before Store takes maintenance EX.
type RootContextReconciliationGate interface {
	WithQuiescentRoot(context.Context, RootPin, func(context.Context) error) error
}

type rootContextSnapshot struct {
	result RootContextReconciliation
	pin    RootPin
	record *task.RootRecord
	data   []byte
}

func rootContextCoordinates(pin RootPin) task.RootContextCoordinates {
	return task.RootContextCoordinates{Device: pin.Device(), Inode: pin.Inode()}
}

func (s *Store) readRootContextRecord(ctx context.Context) (*task.RootRecord, []byte, error) {
	data, err := s.readBytesContext(ctx, filepath.Join(s.Root, "root.json"), task.MaxControlRecordSize)
	if err != nil {
		return nil, nil, err
	}
	record, err := decodeRootContextRecord(data)
	if err != nil {
		return nil, nil, err
	}
	if record.RootID != s.RootID {
		return nil, nil, task.ErrIdentityMismatch
	}
	return record, data, nil
}

func decodeRootContextRecord(data []byte) (*task.RootRecord, error) {
	var record task.RootRecord
	if err := task.DecodeStrict(data, &record); err != nil {
		return nil, err
	}
	if err := task.ValidateRootRecord(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

// PrepareRootContextReconciliation reads existing metadata only. It creates no
// locks and performs no barriers, repairs or writes. Confirmation rechecks it.
func (r *ReadOnlyStore) PrepareRootContextReconciliation(ctx context.Context, expectedToken string, origin task.RootContextCoordinates) (RootContextReconciliation, error) {
	if r == nil || r.store == nil {
		return RootContextReconciliation{}, os.ErrInvalid
	}
	snapshot, err := r.store.prepareRootContextReconciliation(ctx, expectedToken, origin)
	return snapshot.result, err
}

// RootContextOrigin reads a binding's original preimage, or recognizes a legacy
// token that already matches this acquisition. A changed legacy token still
// needs historical evidence. This does not adopt an old acquisition as current.
func (r *ReadOnlyStore) RootContextOrigin(ctx context.Context, expectedToken string) (origin task.RootContextCoordinates, found bool, resultErr error) {
	if err := task.ValidateSHA256(expectedToken); err != nil {
		return origin, false, err
	}
	if r == nil || r.store == nil {
		return origin, false, os.ErrInvalid
	}
	s := r.store
	pin, err := s.PinRootContext(ctx)
	if err != nil {
		return origin, false, err
	}
	record, _, err := s.readRootContextRecord(ctx)
	if err != nil {
		return origin, false, err
	}
	origin = rootContextCoordinates(pin)
	if record.ContextBinding != nil {
		origin = record.ContextBinding.Origin
	} else {
		currentToken, tokenErr := task.ConnectorRootToken(pin.RootID(), origin)
		if tokenErr != nil {
			return origin, false, tokenErr
		}
		if currentToken != expectedToken {
			return task.RootContextCoordinates{}, false, nil
		}
	}
	if err = validateRootContextPreimage(pin, record, expectedToken, origin); err != nil {
		return origin, false, err
	}
	current, err := s.PinRootContext(ctx)
	if err != nil || !pin.Equal(current) {
		return origin, false, errors.Join(task.ErrIdentityMismatch, err)
	}
	return origin, true, ctx.Err()
}

func (s *Store) prepareRootContextReconciliation(ctx context.Context, expectedToken string, origin task.RootContextCoordinates) (rootContextSnapshot, error) {
	pin, err := s.PinRootContext(ctx)
	if err != nil {
		return rootContextSnapshot{}, err
	}
	record, data, err := s.readRootContextRecord(ctx)
	if err != nil {
		return rootContextSnapshot{}, err
	}
	if err = validateRootContextPreimage(pin, record, expectedToken, origin); err != nil {
		return rootContextSnapshot{}, err
	}
	if record.ContextBinding == nil && origin == rootContextCoordinates(pin) {
		return rootContextSnapshot{}, ErrRootContextNotRequired
	}
	if err = s.refuseRootContextStages(ctx, s.Root); err != nil {
		return rootContextSnapshot{}, err
	}
	result := RootContextReconciliation{
		RootID: pin.RootID(), Origin: origin, Accepted: rootContextCoordinates(pin),
		TokenSHA256: task.ComputeSHA256([]byte(expectedToken)), RootRecordSHA256: task.ComputeSHA256(data),
	}
	result.HistorySHA256, result.TaskRequests, result.InspectionRequests, err = s.rootContextHistory(ctx, expectedToken)
	if err != nil {
		return rootContextSnapshot{}, err
	}
	result.Challenge, err = rootContextChallenge(pin, result)
	if err != nil {
		return rootContextSnapshot{}, err
	}
	if binding := record.ContextBinding; binding != nil && binding.Accepted == result.Accepted {
		result.AlreadyReconciled, result.Challenge = true, binding.Challenge
	}
	return rootContextSnapshot{result: result, pin: pin, record: record, data: data}, ctx.Err()
}

func validateRootContextPreimage(pin RootPin, record *task.RootRecord, expectedToken string, origin task.RootContextCoordinates) error {
	if err := task.ValidateSHA256(expectedToken); err != nil {
		return err
	}
	token, err := task.ConnectorRootToken(pin.RootID(), origin)
	if err != nil {
		return err
	}
	if token != expectedToken || origin.Inode != pin.Inode() {
		return fmt.Errorf("%w: expected token does not identify the proposed origin and current inode", task.ErrIdentityMismatch)
	}
	if binding := record.ContextBinding; binding != nil && (binding.Root != pin.Root() || binding.Origin != origin) {
		return fmt.Errorf("%w: root context origin and canonical path are immutable", task.ErrIdentityMismatch)
	}
	return nil
}

func rootContextChallenge(pin RootPin, result RootContextReconciliation) (string, error) {
	return identityToken(struct {
		Domain           string                      `json:"domain"`
		Root             string                      `json:"root"`
		RootID           string                      `json:"root_id"`
		Origin           task.RootContextCoordinates `json:"origin"`
		Accepted         task.RootContextCoordinates `json:"accepted"`
		TokenSHA256      string                      `json:"token_sha256"`
		RootRecordSHA256 string                      `json:"root_record_sha256"`
		HistorySHA256    string                      `json:"history_sha256"`
	}{"delegate-root-context-reconciliation-v1", pin.Root(), pin.RootID(), result.Origin, result.Accepted, result.TokenSHA256, result.RootRecordSHA256, result.HistorySHA256})
}

// ConfirmRootContextReconciliation adopts exactly the prepared physical root.
// offlineConfirmed attests closure of external writers; gate independently
// excludes runtime transitions and remains held through durable publication.
func (s *Store) ConfirmRootContextReconciliation(ctx context.Context, expectedToken string, origin task.RootContextCoordinates, challenge string, offlineConfirmed bool, gate RootContextReconciliationGate) (result RootContextReconciliation, resultErr error) {
	if !offlineConfirmed {
		return result, errors.New("offline reconciliation attestation required: stop old writers and frontends and attest no other live copy")
	}
	if gate == nil {
		return result, errors.New("root context reconciliation requires a runtime quiescence gate")
	}
	if err := task.ValidateSHA256(challenge); err != nil {
		return result, err
	}
	pin, err := s.PinRootContext(ctx)
	if err != nil {
		return result, err
	}
	entered := false
	resultErr = gate.WithQuiescentRoot(ctx, pin, func(gated context.Context) (operationErr error) {
		if entered {
			return fmt.Errorf("%w: reconciliation gate entered more than once", task.ErrInvariantFault)
		}
		entered = true
		if err := s.maintLock.LockEXNonblockingContext(gated); err != nil {
			return err
		}
		defer s.finishConnectorMaintenance(&operationErr)
		snapshot, err := s.prepareRootContextReconciliation(gated, expectedToken, origin)
		if err != nil {
			return err
		}
		if !pin.Equal(snapshot.pin) || challenge != snapshot.result.Challenge {
			return fmt.Errorf("%w: reconciliation challenge is stale", task.ErrRequestConflict)
		}
		result = snapshot.result
		if result.AlreadyReconciled {
			return s.barrierRootContextRecord(snapshot.data)
		}
		return s.commitRootContextReconciliation(gated, expectedToken, snapshot)
	})
	if resultErr == nil && !entered {
		resultErr = fmt.Errorf("%w: reconciliation gate omitted the commit scope", task.ErrInvariantFault)
	}
	return result, resultErr
}

func (s *Store) commitRootContextReconciliation(ctx context.Context, expectedToken string, snapshot rootContextSnapshot) error {
	record := *snapshot.record
	record.RequiredFeatures = slices.Clone(record.RequiredFeatures)
	if !slices.Contains(record.RequiredFeatures, task.RootContextBindingFeature) {
		record.RequiredFeatures = append(record.RequiredFeatures, task.RootContextBindingFeature)
	}
	record.ContextBinding = &task.RootContextBinding{
		Version: 1, Root: snapshot.pin.Root(), Origin: snapshot.result.Origin,
		Accepted: snapshot.result.Accepted, Challenge: snapshot.result.Challenge,
		ConfirmedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := task.ValidateRootRecord(&record); err != nil {
		return err
	}
	data, err := marshalControlRecord(record)
	if err != nil {
		return err
	}
	return s.replaceRootContextRecord(ctx, expectedToken, snapshot, data)
}
