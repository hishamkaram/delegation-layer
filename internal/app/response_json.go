package app

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func writeJSONResponse(stdout io.Writer, command string, response *Response, resultErr error) error {
	response.setError(resultErr)
	if (command == "dispatch" || command == "continue") && resultErr != nil {
		if response.Failure == nil {
			failure := dispatchFailure(resultErr, response.TaskRecord)
			response.Failure = &failure
		}
		failure := response.Failure
		response.Error = dispatchFailureMessage(failure.Code)
		if failure.ReasonCode != "" {
			response.Error = publicSupervisorError(response.Error, resultErr).Error()
		}
	}
	return writeJSON(stdout, *response)
}

func writeJSON(w io.Writer, response Response) error {
	data, err := encodeJSONResponse(response)
	if err != nil {
		return err
	}
	if len(data) > MaxJSONResponseBytes {
		for budget := 4096; budget >= 0; budget /= 2 {
			candidate := truncateDiagnostics(response, budget)
			data, err = encodeJSONResponse(candidate)
			if err != nil {
				return err
			}
			if len(data) <= MaxJSONResponseBytes {
				break
			}
			if budget == 0 {
				return ErrJSONResponseTooLarge
			}
		}
	}
	if len(data) > MaxJSONResponseBytes {
		return ErrJSONResponseTooLarge
	}
	return writeAll(w, data)
}

func encodeJSONResponse(response Response) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func truncateDiagnostics(response Response, budget int) Response {
	candidate := response
	var truncated bool
	candidate.Error, truncated = truncateDiagnostic(response.Error, budget)
	candidate.ErrorTruncated = response.ErrorTruncated || truncated
	if response.Stops != nil {
		candidate.Stops = append([]StopResponse(nil), response.Stops...)
		for i := range candidate.Stops {
			candidate.Stops[i].Message, truncated = truncateDiagnostic(response.Stops[i].Message, budget)
			candidate.Stops[i].MessageTruncated = response.Stops[i].MessageTruncated || truncated
		}
	}
	if response.Stop != nil {
		stop := *response.Stop
		stop.Message, truncated = truncateDiagnostic(response.Stop.Message, budget)
		stop.MessageTruncated = response.Stop.MessageTruncated || truncated
		candidate.Stop = &stop
	}
	if response.Diagnostics != nil {
		candidate.Diagnostics = append([]task.OutputDiagnostic(nil), response.Diagnostics...)
		for i := range candidate.Diagnostics {
			candidate.Diagnostics[i].Message, _ = truncateDiagnostic(response.Diagnostics[i].Message, budget)
		}
	}
	return candidate
}

func truncateDiagnostic(value string, budget int) (string, bool) {
	if value == "" || len(value) <= budget {
		return value, false
	}
	const marker = "[truncated]"
	if budget <= len(marker) {
		return marker, true
	}
	limit := budget - len(marker)
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + marker, true
}
