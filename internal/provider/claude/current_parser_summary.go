package claude

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

// claudeJSONSpan is an offset into the callback-scoped stdout evidence. The
// reader itself is deliberately not retained; Evaluate reconstructs these
// spans during a second sealed read after the whole record stream succeeds.
type claudeJSONSpan struct {
	start int64
	end   int64
}

type claudeTextSummary struct {
	spans      []claudeJSONSpan
	digest     [sha256.Size]byte
	length     int64
	nonblank   bool
	invalidNUL bool
	seen       bool
}

type claudeSummaryWriter struct {
	hash       hash.Hash
	length     int64
	nonblank   bool
	invalidNUL bool
}

func (w *claudeSummaryWriter) Write(p []byte) (int, error) {
	if bytes.IndexByte(p, 0) >= 0 {
		w.invalidNUL = true
	}
	if strings.TrimSpace(string(p)) != "" {
		w.nonblank = true
	}
	if _, err := w.hash.Write(p); err != nil {
		return 0, err
	}
	w.length += int64(len(p))
	return len(p), nil
}

func summarizeClaudeValues(values []providerjson.Value, keepSpans bool) (claudeTextSummary, error) {
	writer := &claudeSummaryWriter{hash: sha256.New()}
	summary := claudeTextSummary{seen: true}
	for _, value := range values {
		if keepSpans {
			summary.spans = append(summary.spans, claudeJSONSpan{start: value.Start, end: value.End})
		}
		if err := value.CopyString(writer); err != nil {
			return claudeTextSummary{}, err
		}
	}
	summary.length = writer.length
	summary.nonblank = writer.nonblank
	summary.invalidNUL = writer.invalidNUL
	copy(summary.digest[:], writer.hash.Sum(nil))
	return summary, nil
}

func summarizeClaudeValue(value providerjson.Value, keepSpans bool) (claudeTextSummary, error) {
	return summarizeClaudeValues([]providerjson.Value{value}, keepSpans)
}

func copyClaudeSpans(reader io.Reader, spans []claudeJSONSpan, out io.Writer) error {
	copied := make([]bool, len(spans))
	semanticErr, readErr := providerjson.ReadRecords(reader, func(record providerjson.Value, _ uint64) error {
		for index, span := range spans {
			if copied[index] || span.start < record.Start || span.end > record.End {
				continue
			}
			if err := (providerjson.Value{Reader: record.Reader, Start: span.start, End: span.end}).CopyString(out); err != nil {
				return err
			}
			copied[index] = true
		}
		return nil
	})
	if err := errors.Join(semanticErr, readErr); err != nil {
		return err
	}
	for _, present := range copied {
		if !present {
			return errors.New("selected Claude answer span was not found")
		}
	}
	return nil
}
