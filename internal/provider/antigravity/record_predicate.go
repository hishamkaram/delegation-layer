package antigravity

import (
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const recordPrintContract = `{"version":"print-record-v1","base":"1.2.2-auth2","rules":"validate full JSON syntax; select exact unique status,conversation_id,response,error; skip unknown extensions; stream response; optional invalid usage is unavailable; preserve identity,exit,timeout and ERROR predicates"}` + "\n"

type recordPrintInterpreter struct{ mode string }

// NewRecordPrintInterpreter selects bounded evidence views for new tasks.
func NewRecordPrintInterpreter(mode string) predicate.Interpreter {
	return recordPrintInterpreter{mode: mode}
}

func (v recordPrintInterpreter) Reference() task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: v.mode, Version: "print-record-v1", SHA256: task.ComputeSHA256([]byte(recordPrintContract))}
}

func (v recordPrintInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !input.Seal.Predicate.Equal(v.Reference()) {
		return task.Interpretation{}, fmt.Errorf("%w: antigravity predicate reference mismatch", task.ErrIdentityMismatch)
	}
	return v.evaluate(input, raw, out)
}

func (v recordPrintInterpreter) evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return task.Interpretation{}, err
	}
	if raw == nil || out == nil {
		return task.Interpretation{}, errors.New("nil antigravity evidence or answer sink")
	}
	var stdout stdoutResult
	var warnings []task.OutputDiagnostic
	readErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		document, err := providerjson.Document(reader)
		if err != nil {
			return err
		}
		env, response, diagnostics, semantic := recordEnvelope(document)
		warnings = diagnostics
		stdout.envelope, stdout.semantic = env, semantic
		if semantic == nil {
			return response.CopyString(out)
		}
		var fieldError *providerjson.FieldError
		if errors.As(semantic, &fieldError) {
			return nil
		}
		return semantic
	})
	var timeout bool
	stderrErr := raw.Read(predicate.Stderr, func(reader io.Reader) error { var err error; timeout, err = scanTimeout(reader); return err })
	if err := errors.Join(readErr, stderrErr); err != nil {
		return task.Interpretation{}, err
	}
	result := interpretEnvelope(input, stdout, timeout, true)
	if result.Verdict == task.VerdictRejected {
		result.Diagnostics = append(result.Diagnostics, recordDiagnostic(stdout.semantic, result.Refusal))
	}
	result.Diagnostics = append(result.Diagnostics, warnings...)
	return result, nil
}

func recordEnvelope(document providerjson.Value) (envelope, providerjson.Value, []task.OutputDiagnostic, error) {
	var env envelope
	fields, err := document.Object("status", "conversation_id", "response", "error")
	if err != nil {
		return env, providerjson.Value{}, nil, err
	}
	for _, name := range []string{"status", "conversation_id", "response"} {
		if _, ok := fields[name]; !ok {
			return env, providerjson.Value{}, nil, &providerjson.FieldError{Code: "missing_control_field", Field: name, Offset: document.Start}
		}
	}
	env.status, err = fields["status"].String(maxSmallFieldBytes)
	if err != nil {
		return env, providerjson.Value{}, nil, err
	}
	env.statusSeen = true
	env.conversationID, err = fields["conversation_id"].String(maxSmallFieldBytes)
	if err != nil {
		return env, providerjson.Value{}, nil, err
	}
	env.conversationSeen = true
	response := fields["response"]
	env.responseNonWS, err = response.Nonblank()
	if err != nil {
		return env, providerjson.Value{}, nil, err
	}
	env.responseSeen = true
	if value, ok := fields["error"]; ok {
		// Native error text is private evidence, not a public diagnostic excerpt.
		if err = value.CopyString(io.Discard); err != nil {
			return env, providerjson.Value{}, nil, err
		}
		env.errorSeen = true
	}
	var warnings []task.OutputDiagnostic
	// Optional accounting cannot overrule valid task output. Parse known members
	// independently from authority fields so extensions and large metadata are skipped.
	optional, usageErr := document.Object("usage", "num_turns", "duration_seconds")
	if usageErr == nil {
		usageErr = recordOptionalUsage(&env, optional)
	}
	if usageErr != nil {
		var fieldError *providerjson.FieldError
		if !errors.As(usageErr, &fieldError) {
			return env, providerjson.Value{}, nil, usageErr
		}
		env.usage = nil
		env.numTurns = nil
		env.durationSeconds = nil
		warnings = []task.OutputDiagnostic{{Code: "optional_usage_unavailable", Message: "Optional provider usage could not be interpreted; the answer was assessed independently.", Field: "usage", Event: 1}}
	}
	return env, response, warnings, nil
}

func recordOptionalUsage(env *envelope, fields map[string]providerjson.Value) error {
	if value, ok := fields["num_turns"]; ok {
		var err error
		env.numTurns, err = recordCounter(value)
		if err != nil {
			return err
		}
	}
	if value, ok := fields["duration_seconds"]; ok {
		var err error
		env.durationSeconds, err = recordDuration(value)
		if err != nil {
			return err
		}
	}
	value, ok := fields["usage"]
	if ok {
		return recordUsage(env, value)
	}
	return nil
}

func recordUsage(env *envelope, value providerjson.Value) error {
	if value.End-value.Start == 4 {
		data, e := value.Raw(4)
		if e != nil {
			return e
		}
		if string(data) == "null" {
			return nil
		}
	}
	usage, err := value.Object("input_tokens", "output_tokens", "thinking_tokens", "cache_read_tokens", "total_tokens")
	if err != nil {
		return err
	}
	usageRecord, err := recordUsageCounters(usage)
	if err != nil {
		return err
	}
	env.usage = usageRecord
	return nil
}

func recordUsageCounters(usage map[string]providerjson.Value) (*usageRecord, error) {
	recorded := &usageRecord{}
	validCounter := false
	counters := []struct {
		name   string
		target **int64
	}{{"input_tokens", &recorded.InputTokens}, {"output_tokens", &recorded.OutputTokens}, {"thinking_tokens", &recorded.ThinkingTokens}, {"cache_read_tokens", &recorded.CacheReadTokens}, {"total_tokens", &recorded.TotalTokens}}
	for _, counter := range counters {
		if field, present := usage[counter.name]; present {
			value, err := recordCounter(field)
			if err != nil {
				return nil, err
			}
			*counter.target = value
			validCounter = validCounter || value != nil
		}
	}
	if !validCounter {
		return nil, nil
	}
	return recorded, nil
}

func recordDuration(value providerjson.Value) (*string, error) {
	data, err := value.Raw(maxNumberBytes)
	if err != nil {
		return nil, err
	}
	if string(data) == "null" {
		return nil, nil
	}
	duration := string(data)
	if !validDurationNumber(duration) {
		return nil, &providerjson.FieldError{Code: "invalid_optional_usage", Field: "duration_seconds", Offset: value.Start}
	}
	return &duration, nil
}

func recordCounter(value providerjson.Value) (*int64, error) {
	data, err := value.Raw(maxNumberBytes)
	if err != nil {
		return nil, err
	}
	if string(data) == "null" {
		return nil, nil
	}
	counter, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil || counter < 0 {
		return nil, &providerjson.FieldError{Code: "invalid_optional_usage", Offset: value.Start}
	}
	return &counter, nil
}

func (v recordPrintInterpreter) Target() task.PredicateRef { return v.Reference() }
func (v recordPrintInterpreter) CanRecover(source task.PredicateRef) bool {
	if source.Equal(NewCurrentPrintInterpreterForMode(v.mode).Reference()) {
		return true
	}
	return v.mode == ModeWorkspaceWrite && source.Equal(NewPrintInterpreter().Reference())
}

func (v recordPrintInterpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !v.CanRecover(input.Seal.Predicate) {
		return task.Interpretation{}, task.ErrIncompatiblePredicate
	}
	return v.evaluate(input, raw, out)
}

func recordDiagnostic(parseErr error, refusal string) task.OutputDiagnostic {
	diagnostic := task.OutputDiagnostic{Code: "provider_output_rejected", Message: "Provider output did not satisfy the recorded completion contract.", Event: 1}
	var fieldError *providerjson.FieldError
	if errors.As(parseErr, &fieldError) {
		diagnostic.Code = fieldError.Code
		diagnostic.Field = fieldError.Field
		diagnostic.Offset = fieldError.Offset
		return diagnostic
	}
	switch refusal {
	case refusalIdentityMismatch:
		diagnostic.Code = "identity_mismatch"
		diagnostic.Message = "Provider conversation identity did not match the task."
	case refusalIncompleteOutput:
		diagnostic.Code = "incomplete_output"
		diagnostic.Message = "Provider output did not contain a complete nonblank answer."
	case refusalInvalidOutput:
		diagnostic.Code = "invalid_output"
		diagnostic.Message = "Provider output contained invalid completion metadata."
	}
	return diagnostic
}
