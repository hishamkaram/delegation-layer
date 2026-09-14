package antigravity

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// This is the non-secret envelope observed from the actual supervised 1.2.2
// invocation on 2026-09-14. Authentication failed before a conversation existed.
func TestObservedAuthenticationFailureCannotPublish(t *testing.T) {
	stdout := []byte(`{"conversation_id":"","status":"ERROR","response":"","error":"authentication failed or timed out","duration_seconds":0,"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0}}`)
	stderr := []byte("Error: authentication required. Run 'agy' to log in, then retry.\nerror: authentication failed or timed out\n")
	interpretation, answer, stdoutReader, stderrReader, err := evaluateAntigravity(t, stdout, stderr, func(seal *task.ProviderExitRecord) {
		seal.ExitCode = 1
	}, task.SessionExpectation{}, nil, 1)
	if err != nil || interpretation.Verdict != task.VerdictRejected || interpretation.Session != nil || len(answer) != 0 {
		t.Fatalf("authentication failure interpretation=%+v answer=%q err=%v", interpretation, answer, err)
	}
	if len(interpretation.Usage) != 1 || interpretation.Usage[0].Reliability != task.UsageReliabilityUnreliable {
		t.Fatalf("authentication failure usage is not marked unreliable: %+v", interpretation.Usage)
	}
	assertReadersDrained(t, stdoutReader, stderrReader, len(stdout), len(stderr))
}
