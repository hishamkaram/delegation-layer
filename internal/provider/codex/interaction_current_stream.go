package codex

import (
	"errors"
	"fmt"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

func currentAppServerFinalText(value providerjson.Value) (providerjson.Value, error) {
	top, err := value.Object("method", "params")
	if err != nil {
		return providerjson.Value{}, err
	}
	method, err := currentRequiredString(top, "method", maxItemTypeBytes)
	if err != nil || method != "item/completed" {
		return providerjson.Value{}, errors.New("selected Codex record is not item/completed")
	}
	params, err := currentParams(top)
	if err != nil {
		return providerjson.Value{}, err
	}
	selected, err := params.Object("item")
	if err != nil {
		return providerjson.Value{}, err
	}
	itemValue, ok := selected["item"]
	if !ok {
		return providerjson.Value{}, errors.New("selected Codex item is missing")
	}
	itemFields, err := itemValue.Object("type", "phase")
	if err != nil {
		return providerjson.Value{}, err
	}
	typeName, err := currentRequiredString(itemFields, "type", maxItemTypeBytes)
	if err != nil || typeName != "agentMessage" {
		return providerjson.Value{}, errors.New("selected Codex item is not an agent message")
	}
	if phaseValue, exists := itemFields["phase"]; exists {
		phase, phaseErr := phaseValue.String(maxItemTypeBytes)
		if phaseErr != nil || phase == "commentary" {
			return providerjson.Value{}, errors.New("selected Codex item is not a final agent message")
		}
	}
	itemFields, err = itemValue.Object("type", "phase", "text")
	if err != nil {
		return providerjson.Value{}, err
	}
	textValue, ok := itemFields["text"]
	if !ok {
		return providerjson.Value{}, errors.New("selected Codex final text is missing")
	}
	return textValue, nil
}

func streamCurrentAppServerFinal(raw predicate.Evidence, start, end int64, out io.Writer) (currentDigest, error) {
	digest := newCurrentDigest()
	writer := &currentOutputWriter{digest: digest, out: out}
	var found bool
	var semanticErr, readErr error
	readErr = raw.Read(predicate.Stdout, func(reader io.Reader) error {
		semanticErr, readErr = providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
			if value.Start != start || value.End != end {
				return nil
			}
			found = true
			textValue, err := currentAppServerFinalText(value)
			if err != nil {
				return err
			}
			return textValue.CopyString(writer)
		})
		return readErr
	})
	if readErr != nil {
		return currentDigest{}, readErr
	}
	if semanticErr != nil {
		var writeErr *currentAnswerWriteError
		if errors.As(semanticErr, &writeErr) {
			return currentDigest{}, writeErr.err
		}
		return currentDigest{}, fmt.Errorf("streaming current Codex interaction answer: %w", semanticErr)
	}
	if !found {
		return currentDigest{}, errors.New("selected Codex interaction answer record was not found")
	}
	return *digest, nil
}
