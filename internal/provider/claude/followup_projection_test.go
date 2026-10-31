package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func responseSection(index int, origin, body string) string {
	return fmt.Sprintf("[delegate response result_index=%d origin=%s bytes=%d]\n%s\n", index, origin, len(body), body)
}

func TestFollowUpProjectionIncludesEveryRootAnswer(t *testing.T) {
	bodies := []string{"progress", "one", "two", "three", strings.Repeat("answer", 900), "housekeeping"}
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		lines := []string{interactiveInitLine(mode)}
		want := "[delegate responses v1]\n"
		for index, body := range bodies {
			attribution, origin := fmt.Sprintf(`,"result_index":%d`, index), "caller"
			if index > 0 {
				attribution, origin = notificationAttribution(index), "task-notification"
			}
			lines = append(lines, attributedResultLine(body, attribution, 1, "0.1"))
			want += responseSection(index, origin, body)
		}
		got, answer := evaluateWriteTrace(t, NewInterpreter(mode), lines)
		if got.Verdict != task.VerdictCommitted || answer != want {
			t.Fatalf("%s: verdict=%s payload bytes=%d want=%d", mode, got.Verdict, len(answer), len(want))
		}
	}
}

func TestFollowUpProjectionPreservesBodiesAndNativeIndexes(t *testing.T) {
	body := " \t世界\n[delegate response result_index=9 origin=caller bytes=1]\n\n"
	lines := []string{writeInitLine(claudeTestUUID), attributedResultLine("", notificationAttribution(0), 0, "0.1"), attributedResultLine(body, `,"result_index":1`, 1, "0.1"), attributedResultLine("", notificationAttribution(2), 0, "0.1"), attributedResultLine(body, notificationAttribution(3), 1, "0.2")}
	got, answer := evaluateWriteTrace(t, NewWorkspaceWriteInterpreter(), lines)
	want := "[delegate responses v1]\n" + responseSection(1, "caller", body) + responseSection(3, "task-notification", body)
	if got.Verdict != task.VerdictCommitted || answer != want {
		t.Fatalf("interpretation=%+v answer=%q", got, answer)
	}
}

func TestFollowUpProjectionSingleAnswerAndFrozenReplay(t *testing.T) {
	body := " \t世界\n[delegate responses v1]\n"
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		lines := []string{interactiveInitLine(mode), attributedResultLine("", notificationAttribution(0), 0, "0.1"), attributedResultLine(body, `,"result_index":1`, 1, "0.1")}
		for _, v := range []predicate.Interpreter{NewInterpreter(mode), newAdvisoryInterpreter(mode), newScopedInterpreter(mode)} {
			got, answer := evaluateWriteTrace(t, v, lines)
			if got.Verdict != task.VerdictCommitted || answer != body || task.ComputeSHA256([]byte(answer)) != task.ComputeSHA256([]byte(body)) {
				t.Fatalf("%+v single-answer bytes changed: %+v %q", v.Reference(), got, answer)
			}
		}
		lines = append(lines, attributedResultLine("later", notificationAttribution(2), 1, "0.2"))
		for _, v := range []predicate.Interpreter{newAdvisoryInterpreter(mode), newScopedInterpreter(mode)} {
			got, answer := evaluateWriteTrace(t, v, lines)
			if got.Verdict != task.VerdictCommitted || answer != body {
				t.Fatalf("%+v replay changed: %+v %q", v.Reference(), got, answer)
			}
		}
	}
}

// expectedResponses describes valid fixture result bodies independently of the
// streaming parser. Rejected fixtures never use this presentation oracle.
func expectedResponses(t *testing.T, lines []string) string {
	t.Helper()
	var bodies []string
	var sections []string
	for _, line := range lines {
		var record struct {
			Type   string `json:"type"`
			Result string `json:"result"`
			Index  int    `json:"result_index"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Type != "result" || strings.TrimSpace(record.Result) == "" {
			continue
		}
		origin := "task-notification"
		if len(bodies) == 0 {
			origin = "caller"
		}
		bodies = append(bodies, record.Result)
		sections = append(sections, responseSection(record.Index, origin, record.Result))
	}
	if len(bodies) == 1 {
		return bodies[0]
	}
	return "[delegate responses v1]\n" + strings.Join(sections, "")
}

func TestFollowUpProjectionBoundsChunksAndEvidencePasses(t *testing.T) {
	lines := []string{writeInitLine(claudeTestUUID), attributedResultLine("principal", `,"result_index":0`, 1, "0.1")}
	for i := 1; i <= 1025; i++ {
		lines = append(lines, attributedResultLine("x", notificationAttribution(i), 1, "0.2"))
	}
	lines = append(lines, attributedResultLine(strings.Repeat("世界", 400000), notificationAttribution(1026), 1, "0.3"))
	stdout := jsonl(lines...)
	raw := &projectionEvidence{claudeTestEvidence: claudeTestEvidence{stdout: stdout}}
	var out claudeCountingWriter
	got, err := NewWorkspaceWriteInterpreter().Evaluate(revisionInput(stdout, WorkspaceWriteReference()), raw, &out)
	want := expectedResponses(t, lines)
	if err != nil || got.Verdict != task.VerdictCommitted || out.count != len(want) || out.max > answerChunkBytes || raw.stdoutReads != 2 {
		t.Fatalf("verdict=%s err=%v bytes=%d/%d max=%d reads=%d", got.Verdict, err, out.count, len(want), out.max, raw.stdoutReads)
	}
}

type projectionEvidence struct {
	claudeTestEvidence
	stdoutReads     int
	projectionError error
}

func (e *projectionEvidence) Read(stream predicate.Stream, consume func(io.Reader) error) error {
	if stream == predicate.Stdout {
		e.stdoutReads++
		if e.stdoutReads == 2 && e.projectionError != nil {
			return e.projectionError
		}
	}
	return e.claudeTestEvidence.Read(stream, consume)
}

func TestFollowUpProjectionFaultsRemainOperational(t *testing.T) {
	stdout := jsonl(nativeFollowUpTrace()...)
	sentinel := errors.New("projection read failed")
	for _, tc := range []struct {
		name string
		out  io.Writer
		raw  *projectionEvidence
		want error
	}{
		{"short writer", &claudeShortWriter{remaining: 180}, &projectionEvidence{claudeTestEvidence: claudeTestEvidence{stdout: stdout}}, io.ErrShortWrite},
		{"failed writer", &claudeFailingWriter{err: sentinel}, &projectionEvidence{claudeTestEvidence: claudeTestEvidence{stdout: stdout}}, sentinel},
		{"failed projection read", io.Discard, &projectionEvidence{claudeTestEvidence: claudeTestEvidence{stdout: stdout}, projectionError: sentinel}, sentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewWorkspaceWriteInterpreter().Evaluate(revisionInput(stdout, WorkspaceWriteReference()), tc.raw, tc.out)
			if !errors.Is(err, tc.want) || got.Verdict != "" {
				t.Fatalf("interpretation=%+v err=%v", got, err)
			}
		})
	}
}

func requireCommittedResponses(t *testing.T, got task.Interpretation, answer string, lines []string) {
	t.Helper()
	if got.Verdict != task.VerdictCommitted || answer != expectedResponses(t, lines) {
		t.Fatalf("%+v answer=%q", got, answer)
	}
}
