package ccr

import (
	"errors"
	"fmt"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	credentialUnavailableExitCode    = 77
	refusalAuthenticationUnavailable = "authentication-unavailable"
)

type credentialInterpreter struct {
	predicate.RecoveryInterpreter
	stream predicate.Interpreter
}

func newCredentialInterpreter(reference, recoverySource task.PredicateRef) (predicate.Interpreter, error) {
	stream, err := claude.NewStreamInterpreterWithRecovery(reference, recoverySource)
	if err != nil {
		return nil, err
	}
	recovery, ok := stream.(predicate.RecoveryInterpreter)
	if !ok {
		return nil, errors.New("CCR stream interpreter does not support recovery")
	}
	return credentialInterpreter{RecoveryInterpreter: recovery, stream: stream}, nil
}

func (v credentialInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	result, err := v.stream.Evaluate(input, raw, out)
	if err != nil {
		return task.Interpretation{}, err
	}
	return classifyCredentialFailure(input, raw, result)
}

func (v credentialInterpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	result, err := v.RecoveryInterpreter.Recover(input, raw, out)
	if err != nil {
		return task.Interpretation{}, err
	}
	return classifyCredentialFailure(input, raw, result)
}

func classifyCredentialFailure(input predicate.Input, raw predicate.Evidence, result task.Interpretation) (task.Interpretation, error) {
	if input.Seal.InvocationState != task.InvocationStarted || input.Seal.ExitCode != credentialUnavailableExitCode ||
		input.Seal.Error != "" || result.Verdict != task.VerdictRejected {
		return result, nil
	}
	var empty bool
	if err := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var first [1]byte
		_, err := io.ReadFull(reader, first[:])
		if errors.Is(err, io.EOF) {
			empty = true
			return nil
		}
		return err
	}); err != nil {
		return task.Interpretation{}, fmt.Errorf("read CCR credential-failure stdout: %w", err)
	}
	if !empty {
		return result, nil
	}
	return task.Interpretation{
		Verdict: task.VerdictRejected,
		Refusal: refusalAuthenticationUnavailable,
		Usage:   result.Usage,
		Diagnostics: []task.OutputDiagnostic{{
			Code:    refusalAuthenticationUnavailable,
			Message: "Provider authentication was unavailable.",
		}},
	}, nil
}

var _ predicate.RecoveryInterpreter = credentialInterpreter{}
