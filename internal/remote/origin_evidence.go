package remote

import (
	"context"
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// OriginAdmissionFacts reads the existing immutable admission intent and its
// exact before-send/rejection evidence. These facts never authorize admission.
type OriginAdmissionFacts struct {
	Intent   Intent
	Sent     bool
	Rejected bool
}

func ReadOriginAdmission(ctx context.Context, reader *taskdir.ReadOnlyStore, id string) (facts OriginAdmissionFacts, err error) {
	err = reader.ReadRemoteRecord(ctx, id, true, false, "intent.json", &facts.Intent)
	if err != nil {
		return facts, err
	}
	if facts.Intent.TaskID != id || facts.Intent.OriginRootID != reader.RootID() {
		return facts, task.ErrIdentityMismatch
	}
	digest, err := IntentDigest(facts.Intent)
	if err != nil {
		return facts, err
	}
	var send SendIntent
	err = reader.ReadRemoteRecord(ctx, id, true, false, "send-intent.json", &send)
	if err == nil {
		if send.IntentSHA256 != digest {
			return facts, ErrConflict
		}
		facts.Sent = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return facts, err
	}
	var rejection admissionRejection
	err = reader.ReadRemoteRecord(ctx, id, true, false, "rejected.json", &rejection)
	if err == nil {
		if rejection.IntentSHA256 != digest {
			return facts, ErrConflict
		}
		facts.Rejected = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return facts, err
	}
	return facts, nil
}
