package antigravity

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestErrorRevisionPreservesLegacyAndSuccessIdentity(t *testing.T) {
	cases := []struct{ name, body, legacy, current string }{
		{"authentication", `{"conversation_id":"","status":"ERROR","response":"","error":"authentication failed"}`, refusalIdentityMismatch, "provider-failed: status=ERROR: authentication failed"},
		{"success needs UUID", `{"conversation_id":"","status":"SUCCESS","response":"answer"}`, refusalIdentityMismatch, refusalIdentityMismatch},
		{"unknown status", `{"conversation_id":"","status":"OTHER","response":""}`, refusalInvalidOutput, refusalInvalidOutput},
		{"duplicate status", `{"conversation_id":"","status":"ERROR","status":"ERROR","response":""}`, refusalInvalidOutput, refusalInvalidOutput},
		{"wrong identity type", `{"conversation_id":null,"status":"ERROR","response":""}`, refusalInvalidOutput, refusalInvalidOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, current := range []bool{false, true} {
				interpreter := NewPrintInterpreter()
				want := tc.legacy
				if current {
					interpreter = NewCurrentPrintInterpreter()
					want = tc.current
				}
				stdout := &trackingReader{data: []byte(tc.body), chunk: 1}
				stderr := &trackingReader{data: []byte("diagnostic"), chunk: 1}
				evidence := &testEvidence{readers: map[predicate.Stream]io.Reader{predicate.Stdout: stdout, predicate.Stderr: stderr}}
				var answer bytes.Buffer
				result, err := interpreter.Evaluate(predicate.Input{Seal: validSeal(interpreter.Reference())}, evidence, &answer)
				if err != nil || result.Verdict != task.VerdictRejected || result.Refusal != want || result.Session != nil {
					t.Fatalf("current=%v result=%+v err=%v want refusal=%s", current, result, err, want)
				}
				assertReadersDrained(t, stdout, stderr, len(tc.body), len("diagnostic"))
			}
		})
	}
	legacy, current := NewPrintInterpreter().Reference(), NewCurrentPrintInterpreter().Reference()
	if legacy.Equal(current) || current.Version != "1.2.2-auth2" || !strings.HasSuffix(currentPrintContract, "\n") {
		t.Fatalf("revisions not distinct: %+v %+v", legacy, current)
	}
}
