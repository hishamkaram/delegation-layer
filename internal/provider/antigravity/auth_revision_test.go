package antigravity

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestErrorRevisionPreservesHistoricalAndCurrentRules(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		legacy     string
		historical string
		current    string
	}{
		{"authentication", `{"conversation_id":"","status":"ERROR","response":"","error":"authentication failed or timed out"}`, refusalIdentityMismatch, "provider-failed: status=ERROR: authentication failed or timed out", refusalAuthenticationUnavailable},
		{"success needs UUID", `{"conversation_id":"","status":"SUCCESS","response":"answer"}`, refusalIdentityMismatch, refusalIdentityMismatch, refusalIdentityMismatch},
		{"unknown status", `{"conversation_id":"","status":"OTHER","response":""}`, refusalInvalidOutput, refusalInvalidOutput, refusalInvalidOutput},
		{"duplicate status", `{"conversation_id":"","status":"ERROR","status":"ERROR","response":""}`, refusalInvalidOutput, refusalInvalidOutput, refusalInvalidOutput},
		{"wrong identity type", `{"conversation_id":null,"status":"ERROR","response":""}`, refusalInvalidOutput, refusalInvalidOutput, refusalInvalidOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			interpreters := []struct {
				name  string
				value predicate.Interpreter
				want  string
			}{
				{name: "legacy", value: NewPrintInterpreter(), want: tc.legacy},
				{name: "historical-current", value: newHistoricalCurrentPrintInterpreterForMode(Mode), want: tc.historical},
				{name: "current", value: NewCurrentPrintInterpreter(), want: tc.current},
			}
			for _, candidate := range interpreters {
				t.Run(candidate.name, func(t *testing.T) {
					stdout := &trackingReader{data: []byte(tc.body), chunk: 1}
					stderr := &trackingReader{data: []byte("diagnostic"), chunk: 1}
					evidence := &testEvidence{readers: map[predicate.Stream]io.Reader{predicate.Stdout: stdout, predicate.Stderr: stderr}}
					var answer bytes.Buffer
					result, err := candidate.value.Evaluate(predicate.Input{Seal: validSeal(candidate.value.Reference())}, evidence, &answer)
					if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != candidate.want || result.Session != nil {
						t.Fatalf("result=%+v err=%v want refusal=%s", result, err, candidate.want)
					}
					assertReadersDrained(t, stdout, stderr, len(tc.body), len("diagnostic"))
				})
			}
		})
	}

	legacy := NewPrintInterpreter().Reference()
	historical := newHistoricalCurrentPrintInterpreterForMode(Mode).Reference()
	current := NewCurrentPrintInterpreter().Reference()
	if legacy.Equal(historical) || historical.Equal(current) || legacy.Equal(current) || historical.Version != "1.2.2-auth2" || current.Version != "1.2.2-auth3" || !strings.HasSuffix(currentPrintContract, "\n") || !strings.HasSuffix(currentAuthPrintContract, "\n") {
		t.Fatalf("revisions not distinct: legacy=%+v historical=%+v current=%+v", legacy, historical, current)
	}
}

func TestCurrentAuthenticationClassificationRequiresExactEnvelope(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		stderr   string
		want     string
		contains string
	}{
		{name: "similar error text", body: `{"conversation_id":"","status":"ERROR","response":"","error":"authentication failed"}`, want: "provider-failed: status=ERROR: authentication failed", contains: "authentication failed"},
		{name: "success status", body: `{"conversation_id":"","status":"SUCCESS","response":"","error":"authentication failed or timed out"}`, want: refusalIdentityMismatch},
		{name: "nonempty response", body: `{"conversation_id":"","status":"ERROR","response":"answer","error":"authentication failed or timed out"}`, want: "provider-failed: status=ERROR: authentication failed or timed out", contains: "authentication failed or timed out"},
		{name: "whitespace response", body: `{"conversation_id":"","status":"ERROR","response":" ","error":"authentication failed or timed out"}`, want: "provider-failed: status=ERROR: authentication failed or timed out", contains: "authentication failed or timed out"},
		{name: "nonempty conversation", body: `{"conversation_id":"` + testConversation + `","status":"ERROR","response":"","error":"authentication failed or timed out"}`, want: "provider-failed: status=ERROR: authentication failed or timed out", contains: "authentication failed or timed out"},
		{name: "stderr only", body: `{"conversation_id":"","status":"ERROR","response":""}`, stderr: authenticationErrorText, want: "provider-failed: status=ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _, stdout, stderr, err := evaluateInterpreter(t, NewCurrentPrintInterpreter(), []byte(tc.body), []byte(tc.stderr), nil, task.SessionExpectation{}, nil, 1)
			if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != tc.want || result.Session != nil {
				t.Fatalf("result=%+v err=%v want=%q", result, err, tc.want)
			}
			if tc.contains != "" && !strings.Contains(result.Refusal, tc.contains) {
				t.Fatalf("refusal=%q lost expected provider failure context", result.Refusal)
			}
			assertReadersDrained(t, stdout, stderr, len(tc.body), len(tc.stderr))
		})
	}
}
