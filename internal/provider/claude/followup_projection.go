package claude

import (
	"errors"
	"fmt"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

// claudeSelectedAnswer is one reducer step's selection. The reducer owns it;
// projection consumes it before the next record, retaining no answer history.
type claudeSelectedAnswer struct {
	index  int64
	origin string
	text   claudeTextSummary
}

// writeClaudeCurrentAnswer publishes a validated current stream's answer: the
// principal's exact bytes, or every selected answer when the policy projects
// follow-ups and the validation pass accepted an answer-bearing follow-up.
func writeClaudeCurrentAnswer(reader io.Reader, state eventState, policy completionPolicy, out io.Writer) error {
	if state.attribution != nil && state.attribution.project && state.attribution.hasFollowUpAnswers {
		return projectClaudeResponses(reader, state.mode, policy, out)
	}
	return copyClaudeSpans(reader, state.result.currentAnswer.spans, out)
}

// projectClaudeResponses runs only after full validation of sealed evidence
// under policy, the caller's immutable completion policy, which must select
// answers. It uses the same decoder and reducer for selection, and copies each
// selected string from the current record before that record's reader expires.
func projectClaudeResponses(reader io.Reader, mode string, policy completionPolicy, out io.Writer) error {
	if err := writeAnswer(out, []byte("[delegate responses v1]\n")); err != nil {
		return err
	}
	state := eventState{mode: mode, current: true, attribution: newCompletionAttribution(policy)}
	if state.attribution == nil || !state.attribution.project {
		return errors.New("claude completion policy does not select follow-up answers")
	}
	semanticErr, readErr := providerjson.ReadRecords(reader, func(record providerjson.Value, _ uint64) error {
		if err := processClaudeCurrentRecord(&state, record); err != nil {
			return err
		}
		selected := state.attribution.selected
		if selected == nil {
			return nil
		}
		header := fmt.Sprintf("[delegate response result_index=%d origin=%s bytes=%d]\n", selected.index, selected.origin, selected.text.length)
		if err := writeAnswer(out, []byte(header)); err != nil {
			return err
		}
		for _, span := range selected.text.spans {
			if err := (providerjson.Value{Reader: record.Reader, Start: span.start, End: span.end}).CopyString(out); err != nil {
				return err
			}
		}
		return writeAnswer(out, []byte("\n"))
	})
	return errors.Join(semanticErr, readErr)
}
