// Package resultread verifies committed payloads for bounded reads or caller-owned files.
package resultread

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const MaxRangeBytes int64 = 65536

// Range retains only selected bytes; the descriptor remains the publication owner's value.
type Range struct {
	Descriptor task.PayloadDescriptor `json:"descriptor"`
	Offset     int64                  `json:"offset"`
	Data       []byte                 `json:"data_base64"`
	NextOffset int64                  `json:"next_offset"`
	EOF        bool                   `json:"eof"`
}

// Read verifies length and digest including all undisplayed bytes. On any
// failure, including cancellation after the final read, no range bytes escape.
func Read(ctx context.Context, source io.Reader, descriptor task.PayloadDescriptor, offset, limit int64) (Range, error) {
	if ctx == nil {
		return Range{}, context.Canceled
	}
	if err := task.ValidatePayloadDescriptor(descriptor); err != nil {
		return Range{}, err
	}
	if limit < 1 || limit > MaxRangeBytes || offset < 0 || offset > descriptor.Length {
		return Range{}, errors.New("result range requires offset within payload and limit between 1 and 65536")
	}
	verification := payloadVerification{expectedLength: descriptor.Length, expectedSHA256: descriptor.SHA256, offset: offset, end: offset + min(limit, descriptor.Length-offset), digest: sha256.New(), selected: []byte{}}
	if err := verification.scan(ctx, source); err != nil {
		return Range{}, err
	}
	next := offset + int64(len(verification.selected))
	return Range{Descriptor: descriptor, Offset: offset, Data: verification.selected, NextOffset: next, EOF: next == descriptor.Length}, nil
}

// copyVerified verifies the complete payload while copying to a private staging writer.
// Save publishes staged bytes only after verification and both file closes succeed.
func copyVerified(ctx context.Context, source io.Reader, descriptor task.PayloadDescriptor, destination io.Writer) error {
	if destination == nil {
		return errors.New("result destination writer is required")
	}
	if ctx == nil {
		return context.Canceled
	}
	if err := task.ValidatePayloadDescriptor(descriptor); err != nil {
		return err
	}
	verification := payloadVerification{expectedLength: descriptor.Length, expectedSHA256: descriptor.SHA256, digest: sha256.New(), destination: destination}
	return verification.scan(ctx, source)
}

// One invocation owns incremental verification state and its bounded selection.
type payloadVerification struct {
	expectedLength      int64
	expectedSHA256      string
	offset, end, length int64
	digest              hash.Hash
	selected            []byte
	destination         io.Writer
}

func (v *payloadVerification) scan(ctx context.Context, source io.Reader) error {
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := source.Read(buffer)
		if n > 0 {
			if consumeErr := v.consume(buffer[:n]); consumeErr != nil {
				return consumeErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.length != v.expectedLength || hex.EncodeToString(v.digest.Sum(nil)) != v.expectedSHA256 {
		return task.ErrEvidenceFault
	}
	return nil
}

func (v *payloadVerification) consume(data []byte) error {
	if int64(len(data)) > v.expectedLength-v.length {
		return task.ErrEvidenceFault
	}
	if _, err := v.digest.Write(data); err != nil {
		return err
	}
	if v.destination != nil {
		n, err := v.destination.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
	}
	next := v.length + int64(len(data))
	begin, end := max(v.length, v.offset), min(next, v.end)
	if begin < end {
		v.selected = append(v.selected, data[begin-v.length:end-v.length]...)
	}
	v.length = next
	return nil
}
