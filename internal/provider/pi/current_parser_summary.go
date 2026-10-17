package pi

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

type piTextSummary struct {
	digest   [sha256.Size]byte
	length   int64
	nonblank bool
	seen     bool
}

type piSummaryWriter struct {
	hash     hash.Hash
	length   int64
	nonblank bool
}

func (w *piSummaryWriter) Write(p []byte) (int, error) {
	if strings.TrimSpace(string(p)) != "" {
		w.nonblank = true
	}
	if _, err := w.hash.Write(p); err != nil {
		return 0, err
	}
	w.length += int64(len(p))
	return len(p), nil
}

func (w *piSummaryWriter) summary() piTextSummary {
	var summary piTextSummary
	summary.length = w.length
	summary.nonblank = w.nonblank
	copy(summary.digest[:], w.hash.Sum(nil))
	summary.seen = true
	return summary
}

func summarizePiValues(values []providerjson.Value, _ bool) (piTextSummary, error) {
	writer := &piSummaryWriter{hash: sha256.New()}
	for _, value := range values {
		if err := value.CopyString(writer); err != nil {
			return piTextSummary{}, err
		}
	}
	return writer.summary(), nil
}

func equalPiText(left, right piTextSummary) bool {
	return left.seen == right.seen && left.length == right.length && left.digest == right.digest
}

func copyPiCurrentMessageText(reader io.Reader, start, end int64, out io.Writer) error {
	if start < 0 || end <= start {
		return errors.New("pi turn answer range is invalid")
	}
	var found bool
	semanticErr, readErr := providerjson.ReadRecords(reader, func(record providerjson.Value, _ uint64) error {
		if start < record.Start || end > record.End {
			return nil
		}
		found = true
		return copyPiMessageValue(providerjson.Value{Reader: record.Reader, Start: start, End: end}, out)
	})
	if err := errors.Join(semanticErr, readErr); err != nil {
		return err
	}
	if !found {
		return errors.New("selected Pi turn answer record was not found")
	}
	return nil
}

func copyPiMessageValue(message providerjson.Value, out io.Writer) error {
	fields, err := message.Object("role", "content")
	if err != nil {
		return err
	}
	content, ok := fields["content"]
	if !ok {
		return errors.New("pi turn answer content is missing")
	}
	if err := content.CopyString(out); err == nil {
		return nil
	} else {
		var fieldErr *providerjson.FieldError
		if !errors.As(err, &fieldErr) {
			return err
		}
	}
	return content.Array(func(block providerjson.Value) error {
		blockFields, err := block.Object("type")
		if err != nil {
			return err
		}
		kind, err := piCurrentRequiredString(blockFields, "type", 128)
		if err != nil {
			return err
		}
		if kind != "text" {
			return nil
		}
		textFields, err := block.Object("text")
		if err != nil {
			return err
		}
		text, ok := textFields["text"]
		if !ok {
			return errors.New("pi text content block is missing text")
		}
		return text.CopyString(out)
	})
}
