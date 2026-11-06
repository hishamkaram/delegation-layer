package infrastructure

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func contextName(reference LaunchContextReference) (string, error) {
	if err := reference.Validate(); err != nil {
		return "", err
	}
	return "context-" + reference.SHA256[:32] + ".json", nil
}

func contextChunkName(digest string) (string, error) {
	if task.ValidateSHA256(digest) != nil {
		return "", ErrIdentity
	}
	return "envchunk-" + digest[:32] + ".json", nil
}

// stageContext publishes bounded immutable chunks tail first and the manifest last.
// Partial chunks are inert content, retained until root disposal. This operation
// borrows no instance lease and never owns native launch or lifecycle policy.
func (j *intentJournal) stageContext(ctx context.Context, record LaunchContext) (reference LaunchContextReference, created bool, resultErr error) {
	record = record.Clone()
	resultErr = j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		manifest, chunks, err := encodeLaunchContext(record)
		if err != nil {
			return err
		}
		if manifest.Root != j.pin.Root() || manifest.RootID != j.pin.RootID() {
			return ErrIdentity
		}
		digest, err := digestStartupRecord(manifest)
		if err != nil {
			return err
		}
		reference = LaunchContextReference{SHA256: digest}
		if err = j.stageContextChunks(ctx, chunks); err != nil {
			return err
		}
		name, err := contextName(reference)
		if err != nil {
			return err
		}
		created, err = publishContextRecord(ctx, j, name, manifest)
		return err
	})
	return reference, created, resultErr
}

// Called inside the journal borrow. Tail-first publication makes every
// published link point to content already verified by the canonical writer.
func (j *intentJournal) stageContextChunks(ctx context.Context, chunks []contextChunk) error {
	for index := len(chunks) - 1; index >= 0; index-- {
		chunk := chunks[index]
		digest, err := digestStartupRecord(chunk)
		if err != nil {
			return err
		}
		name, err := contextChunkName(digest)
		if err != nil {
			return err
		}
		if _, err = publishContextRecord(ctx, j, name, chunk); err != nil {
			return err
		}
	}
	return nil
}

func publishContextRecord[T contextChunk | contextManifest](ctx context.Context, journal *intentJournal, name string, record T) (created bool, resultErr error) {
	if err := journal.checkRoot(ctx); err != nil {
		return false, err
	}
	resultErr = journal.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := journal.checkRoot(ctx); err != nil {
			return err
		}
		var err error
		created, err = tx.Put(name, record)
		return err
	})
	if resultErr != nil {
		return created, resultErr
	}
	return created, journal.checkRoot(ctx)
}

func (j *intentJournal) readContext(ctx context.Context, reference LaunchContextReference) (record LaunchContext, resultErr error) {
	resultErr = j.withOpen(ctx, func() error {
		if err := j.checkRoot(ctx); err != nil {
			return err
		}
		var err error
		record, err = readContextRecords(ctx, controlContextReader{j.control}, reference, j.pin)
		if err != nil {
			return err
		}
		return j.checkRoot(ctx)
	})
	return record, resultErr
}

func readLaunchContext(ctx context.Context, store *taskdir.ReadOnlyStore, reference LaunchContextReference) (record LaunchContext, resultErr error) {
	resultErr = withPassiveOwnerRecords(ctx, store, func(_ ownerRecordReader, pin taskdir.RootPin) error {
		var err error
		record, err = readContextRecords(ctx, passiveContextReader{store}, reference, pin)
		return err
	})
	return record, resultErr
}

type contextRecordReader interface {
	manifest(context.Context, string) (contextManifest, error)
	chunk(context.Context, string) (contextChunk, error)
}

type controlContextReader struct{ control *taskdir.ControlDir }

func (r controlContextReader) manifest(ctx context.Context, name string) (contextManifest, error) {
	var record contextManifest
	if err := ctx.Err(); err != nil {
		return record, err
	}
	err := r.control.Read(name, &record)
	return record, err
}

func (r controlContextReader) chunk(ctx context.Context, name string) (contextChunk, error) {
	var record contextChunk
	if err := ctx.Err(); err != nil {
		return record, err
	}
	err := r.control.Read(name, &record)
	return record, err
}

type transactionContextReader struct{ transaction *taskdir.ControlTransaction }

func (r transactionContextReader) manifest(ctx context.Context, name string) (contextManifest, error) {
	var record contextManifest
	if err := ctx.Err(); err != nil {
		return record, err
	}
	err := r.transaction.Read(name, &record)
	return record, err
}

func (r transactionContextReader) chunk(ctx context.Context, name string) (contextChunk, error) {
	var record contextChunk
	if err := ctx.Err(); err != nil {
		return record, err
	}
	err := r.transaction.Read(name, &record)
	return record, err
}

type passiveContextReader struct{ store *taskdir.ReadOnlyStore }

func (r passiveContextReader) manifest(ctx context.Context, name string) (contextManifest, error) {
	var record contextManifest
	data, err := r.store.ReadInfrastructureRecordContext(ctx, name)
	if err != nil {
		return record, err
	}
	err = task.DecodeStrict(data, &record)
	return record, err
}

func (r passiveContextReader) chunk(ctx context.Context, name string) (contextChunk, error) {
	var record contextChunk
	data, err := r.store.ReadInfrastructureRecordContext(ctx, name)
	if err != nil {
		return record, err
	}
	err = task.DecodeStrict(data, &record)
	return record, err
}

func readContextRecords(ctx context.Context, reader contextRecordReader, reference LaunchContextReference, pin taskdir.RootPin) (record LaunchContext, resultErr error) {
	defer func() {
		if resultErr != nil {
			record = LaunchContext{}
			if errors.Is(resultErr, context.Canceled) || errors.Is(resultErr, context.DeadlineExceeded) {
				resultErr = redactedContextReadError{cause: resultErr}
				return
			}
			resultErr = fmt.Errorf("%w: %w", task.ErrEvidenceFault, redactedContextReadError{cause: resultErr})
		}
	}()
	name, err := contextName(reference)
	if err != nil {
		return record, err
	}
	manifest, err := reader.manifest(ctx, name)
	if err != nil {
		return record, err
	}
	if err = manifest.validate(); err != nil {
		return record, err
	}
	digest, err := digestStartupRecord(manifest)
	if err != nil {
		return record, err
	}
	if digest != reference.SHA256 || manifest.Root != pin.Root() || manifest.RootID != pin.RootID() {
		return record, ErrIdentity
	}
	total, err := manifest.payloadBytes()
	if err != nil {
		return record, err
	}
	// Grow only from verified actual chunk bytes, never a declared aggregate.
	payload := []byte{}
	next := manifest.Head
	for remaining := contextChunkCount(total); remaining > 0; remaining-- {
		chunk, readErr := readVerifiedContextChunk(ctx, reader, next, min(contextChunkBytes, total-len(payload)), remaining == 1)
		if readErr != nil {
			return record, readErr
		}
		payload = append(payload, chunk.Data...)
		next = chunk.Next
	}

	if err = ctx.Err(); err != nil {
		return record, err
	}
	return decodeContextPayload(manifest, payload)
}

func readVerifiedContextChunk(ctx context.Context, reader contextRecordReader, digest string, expected int, last bool) (contextChunk, error) {
	var empty contextChunk
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	name, err := contextChunkName(digest)
	if err != nil {
		return empty, err
	}
	chunk, err := reader.chunk(ctx, name)
	if err != nil {
		return empty, err
	}
	actual, err := digestStartupRecord(chunk)
	if err != nil {
		return empty, err
	}
	if chunk.Protocol != RecordProtocolVersion || len(chunk.Data) != expected || actual != digest {
		return empty, ErrIdentity
	}
	if last {
		if chunk.Next != "" {
			return empty, ErrIdentity
		}
	} else if task.ValidateSHA256(chunk.Next) != nil {
		return empty, ErrIdentity
	}
	return chunk, nil
}

// Causes remain available for classification, but decoder/domain diagnostics
// may quote payload bytes or keys and therefore never render through Error.
type redactedContextReadError struct{ cause error }

func (redactedContextReadError) Error() string {
	return "infrastructure launch context unavailable or invalid"
}
func (e redactedContextReadError) Unwrap() error { return e.cause }
