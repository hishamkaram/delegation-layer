package taskdir

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestProviderAccountingPublicationReplayAndCopy(t *testing.T) {
	for _, verdict := range []string{task.VerdictCommitted, task.VerdictRejected} {
		t.Run(verdict, func(t *testing.T) {
			runProviderAccountingCase(t, verdict)
		})
	}
}

func runProviderAccountingCase(t *testing.T, verdict string) {
	t.Helper()
	input, output, thinking, duration := int64(0), int64(25), int64(13), "1.250"
	usage := []task.UsageMetadata{{
		Scope:           task.UsageScopeConversationCumulative,
		Source:          task.UsageSourceProviderEnvelope,
		Reliability:     task.UsageReliabilityReported,
		InputTokens:     &input,
		OutputTokens:    &output,
		ThinkingTokens:  &thinking,
		DurationSeconds: &duration,
	}}
	if verdict == task.VerdictRejected {
		usage[0].Reliability = task.UsageReliabilityUnreliable
	}
	fn := func(in predicate.Input, evidence predicate.Evidence, out io.Writer) (task.Interpretation, error) {
		result, err := streamInterpreter(in, evidence, out)
		result.Verdict = verdict
		result.Usage = usage
		if verdict == task.VerdictRejected {
			result.Refusal = "incomplete-output"
		}
		return result, err
	}
	_, td := interpreterTask(t, fn, "answer")
	winner := collectInterpreter(t, td)
	assertPublishedAccounting(t, winner, verdict, duration)
	original := readTestFile(t, filepath.Join(td.Dir, "outcome.json"))
	*usage[0].InputTokens = 99
	*usage[0].ThinkingTokens = 101
	usage[0].Scope = task.UsageScopeTask
	assertPublishedAccountingUnchanged(t, winner, verdict, duration)
	recollected := collectInterpreter(t, td)
	if !task.CompareOutcomes(winner, recollected) || !bytes.Equal(original, readTestFile(t, filepath.Join(td.Dir, "outcome.json"))) {
		t.Fatal("recollection changed immutable accounting or outcome bytes")
	}
	var saved task.OutcomeRecord
	if err := task.DecodeStrict(original, &saved); err != nil {
		t.Fatal(err)
	}
	if !task.CompareOutcomes(winner, &saved) {
		t.Fatal("persisted accounting differs from returned winner")
	}
}

func assertPublishedAccounting(t *testing.T, outcome *task.OutcomeRecord, verdict, duration string) {
	t.Helper()
	if len(outcome.Usage) != 1 {
		t.Fatal("published accounting scope count changed")
	}
	usage := outcome.Usage[0]
	assertUsageCounter(t, "input", usage.InputTokens, 0)
	assertUsageCounter(t, "output", usage.OutputTokens, 25)
	assertUsageCounter(t, "thinking", usage.ThinkingTokens, 13)
	if usage.DurationSeconds == nil || *usage.DurationSeconds != duration {
		t.Fatal("published duration changed")
	}
	if verdict == task.VerdictRejected && usage.Reliability != task.UsageReliabilityUnreliable {
		t.Fatal("rejected accounting reliability changed")
	}
}

func assertPublishedAccountingUnchanged(t *testing.T, outcome *task.OutcomeRecord, verdict, duration string) {
	t.Helper()
	assertPublishedAccounting(t, outcome, verdict, duration)
	if *outcome.Usage[0].InputTokens != 0 || *outcome.Usage[0].ThinkingTokens != 13 || outcome.Usage[0].Scope != task.UsageScopeConversationCumulative {
		t.Fatal("interpreter retained mutable outcome accounting")
	}
}

func assertUsageCounter(t *testing.T, name string, value *int64, expected int64) {
	t.Helper()
	if value == nil || *value != expected {
		t.Fatalf("published %s counter changed: got %v, want %d", name, value, expected)
	}
}

func TestProviderAccountingRetainsSeparateScopesAndReset(t *testing.T) {
	cumulative, reset, mainAgent, wholeTree := int64(120), int64(3), int64(9), int64(15)
	usage := []task.UsageMetadata{
		{Scope: task.UsageScopeConversationCumulative, Source: task.UsageSourceProviderEnvelope, Reliability: task.UsageReliabilityReported, TotalTokens: &cumulative},
		{Scope: task.UsageScopeConversationCumulative, Source: task.UsageSourceProviderEnvelope, Reliability: task.UsageReliabilityReported, TotalTokens: &reset},
		{Scope: task.UsageScopeMainAgent, Source: task.UsageSourceProviderEvent, Reliability: task.UsageReliabilityReported, TotalTokens: &mainAgent},
		{Scope: task.UsageScopeWholeTree, Source: task.UsageSourceProviderEvent, Reliability: task.UsageReliabilityReported, TotalTokens: &wholeTree},
	}
	fn := func(in predicate.Input, evidence predicate.Evidence, out io.Writer) (task.Interpretation, error) {
		result, err := streamInterpreter(in, evidence, out)
		result.Usage = usage
		return result, err
	}
	_, td := interpreterTask(t, fn, "separate scopes")
	winner := collectInterpreter(t, td)
	if len(winner.Usage) != len(usage) {
		t.Fatalf("usage scope count changed: got %d", len(winner.Usage))
	}
	for i := range usage {
		if winner.Usage[i].Scope != usage[i].Scope || winner.Usage[i].TotalTokens == nil || *winner.Usage[i].TotalTokens != *usage[i].TotalTokens {
			t.Fatalf("usage scope %d was merged or changed: %+v", i, winner.Usage[i])
		}
	}
}

func TestInvalidProviderAccountingCannotPublish(t *testing.T) {
	fn := func(in predicate.Input, evidence predicate.Evidence, out io.Writer) (task.Interpretation, error) {
		result, err := streamInterpreter(in, evidence, out)
		result.Usage = []task.UsageMetadata{{Scope: "made-up", Source: task.UsageSourceProviderEnvelope, Reliability: task.UsageReliabilityReported}}
		return result, err
	}
	_, td := interpreterTask(t, fn, "answer")
	result, cleanup, err := td.Collect(testInterpreterRef())
	must(t, cleanup)
	if result != nil || err == nil {
		t.Fatal("invalid provider accounting became a terminal result")
	}
	if errors.Is(err, task.ErrInvariantFault) {
		t.Fatalf("invalid accounting was misclassified as a storage invariant fault: %v", err)
	}
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
	testAbsent(t, filepath.Join(td.Dir, "result.txt"))
	testAbsent(t, filepath.Join(td.Dir, "publish.reject"))
}
