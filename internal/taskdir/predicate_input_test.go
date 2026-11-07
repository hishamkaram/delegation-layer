package taskdir

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

const probeConversation = "saved-conversation"

// inputProbe records a snapshot of every Input it receives, then mutates the
// nested review target it was handed the way a careless adapter could. Registered as the
// rejecting source it drives publication; registered as the recovery target it
// drives offline recheck.
type inputProbe struct {
	ref      task.PredicateRef
	recovers bool
	inputs   []predicate.Input
}

func (p *inputProbe) Reference() task.PredicateRef { return p.ref }
func (p *inputProbe) Target() task.PredicateRef    { return p.ref }
func (p *inputProbe) CanRecover(task.PredicateRef) bool {
	return p.recovers
}

func (p *inputProbe) observe(input predicate.Input) {
	received := input
	if target := input.RequestedConfig.ReviewTarget; target != nil {
		snapshot := *target
		received.RequestedConfig.ReviewTarget = &snapshot
	}
	p.inputs = append(p.inputs, received)
	if target := input.RequestedConfig.ReviewTarget; target != nil {
		target.Kind = task.ReviewTargetCommit
		target.Value = "mutated-by-interpreter"
	}
}

func (p *inputProbe) Evaluate(input predicate.Input, _ predicate.Evidence, _ io.Writer) (task.Interpretation, error) {
	p.observe(input)
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: "probe refusal"}, nil
}

func (p *inputProbe) Recover(input predicate.Input, _ predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	p.observe(input)
	_, err := io.WriteString(out, "recovered")
	return task.Interpretation{Verdict: task.VerdictCommitted}, err
}

type probedTask struct {
	td       *TaskDir
	source   *inputProbe
	recovery *inputProbe
	request  *task.TaskRecord
}

// sealedProbeTask creates a real sealed task whose immutable request carries
// config. A non-empty conversation makes it a continuation with a recorded
// provider identity.
func sealedProbeTask(t *testing.T, config task.TaskConfig, conversation string) probedTask {
	t.Helper()
	source := &inputProbe{ref: testInterpreterRef()}
	recovery := &inputProbe{ref: recheckTestRef("codex:exec", "recovery-v1"), recovers: true}
	registry, err := predicate.New(source, recovery)
	must(t, err)
	store, err := InitStoreWithPredicates(filepath.Join(t.TempDir(), "state"), registry)
	must(t, err)
	t.Cleanup(func() { must(t, store.Close()) })
	req, meta, brief := preparedInput(t, store)
	req.RequestedConfig = config
	meta.RequestedConfig = config
	meta.Predicate = source.ref
	if conversation != "" {
		predecessor, idErr := randomid.NewTaskID()
		must(t, idErr)
		req.PriorSession = &task.PriorSession{Provider: req.Provider, ConversationID: conversation, PredecessorTaskID: predecessor}
	}
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, td.Close()) })
	if conversation != "" {
		must(t, td.ClaimSession(req.Provider, conversation))
	}
	permit := consumeStart(t, td)
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if conversation != "" {
		must(t, td.RecordProviderIdentity(task.SessionIdentity{Provider: req.Provider, ConversationID: conversation}))
	}
	must(t, td.RecordStarted(1))
	if config.Interactive {
		must(t, td.BeginInteraction())
		must(t, td.RecordInteraction(task.InteractionEvent{Kind: "ready", TurnID: "turn-1", Capabilities: &task.InteractionCapabilities{Steering: true}}))
		must(t, td.SealInteraction())
	}
	must(t, td.WriteRawFiles("source raw", "source stderr"))
	_, err = td.Seal(task.InvocationStarted, 0, "", source.ref)
	must(t, err)
	must(t, permit.Release())
	return probedTask{td: td, source: source, recovery: recovery, request: req}
}

func requireRequestedConfig(t *testing.T, got, want task.TaskConfig) {
	t.Helper()
	gotTarget, wantTarget := got.ReviewTarget, want.ReviewTarget
	got.ReviewTarget, want.ReviewTarget = nil, nil
	if got != want {
		t.Fatalf("requested config scalars = %+v, want %+v", got, want)
	}
	if (gotTarget == nil) != (wantTarget == nil) || (wantTarget != nil && *gotTarget != *wantTarget) {
		t.Fatalf("review target = %+v, want %+v", gotTarget, wantTarget)
	}
}

func requireSealAndSessions(t *testing.T, name string, input predicate.Input, probed probedTask, conversation string) {
	t.Helper()
	if !input.Seal.Predicate.Equal(probed.source.ref) || len(input.Seal.RawManifest) == 0 {
		t.Fatalf("%s seal changed: %+v", name, input.Seal)
	}
	if conversation == "" {
		if input.ExpectedSession != (task.SessionExpectation{}) || input.RecordedSession != nil {
			t.Fatalf("%s invented session context: %+v %+v", name, input.ExpectedSession, input.RecordedSession)
		}
		return
	}
	if input.ExpectedSession != (task.SessionExpectation{Required: true, ID: conversation}) {
		t.Fatalf("%s expected session = %+v", name, input.ExpectedSession)
	}
	if input.RecordedSession == nil || *input.RecordedSession != (task.SessionIdentity{Provider: probed.request.Provider, ConversationID: conversation}) {
		t.Fatalf("%s recorded session = %+v", name, input.RecordedSession)
	}
}

func TestInterpretersReceiveRequestedConfigFromPreparedRequest(t *testing.T) {
	supplied := task.TaskConfig{
		Interactive:   true,
		Model:         "probe-model",
		Effort:        "high",
		Permission:    "read-only",
		Budget:        "1m0s",
		NativeTimeout: "30s",
		ReviewTarget:  &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "main"},
	}
	defaults := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	for _, tc := range []struct {
		name         string
		config       task.TaskConfig
		conversation string
	}{
		{name: "every supplied field on a continuation", config: supplied, conversation: probeConversation},
		{name: "defaults without a continuation", config: defaults},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probed := sealedProbeTask(t, tc.config, tc.conversation)
			storedRequest := readTestFile(t, filepath.Join(probed.td.Dir, "task.json"))

			outcome, cleanup, err := probed.td.Collect(probed.source.ref)
			must(t, cleanup)
			must(t, err)
			if outcome == nil || outcome.Verdict != task.VerdictRejected {
				t.Fatalf("source outcome = %+v", outcome)
			}
			if _, err = probed.td.Recheck(probed.recovery); err != nil {
				t.Fatal(err)
			}
			if len(probed.source.inputs) != 1 || len(probed.recovery.inputs) != 1 {
				t.Fatalf("interpreter calls: publication=%d recheck=%d", len(probed.source.inputs), len(probed.recovery.inputs))
			}
			published, rechecked := probed.source.inputs[0], probed.recovery.inputs[0]

			requireRequestedConfig(t, published.RequestedConfig, tc.config)
			requireRequestedConfig(t, rechecked.RequestedConfig, tc.config)
			requireSealAndSessions(t, "publication", published, probed, tc.conversation)
			requireSealAndSessions(t, "recheck", rechecked, probed, tc.conversation)
			if string(readTestFile(t, filepath.Join(probed.td.Dir, "task.json"))) != string(storedRequest) {
				t.Fatal("interpreter mutation reached the stored request")
			}
		})
	}
}

func TestMalformedPreparedRequestRefusesBeforeInterpreters(t *testing.T) {
	config := task.TaskConfig{Permission: "read-only", Budget: "1m0s", Model: "probe-model"}
	tamper := func(t *testing.T, probed probedTask) {
		t.Helper()
		requestPath := filepath.Join(probed.td.Dir, "task.json")
		writeTestFile(t, requestPath, append(readTestFile(t, requestPath), '\n'))
	}
	t.Run("publication", func(t *testing.T) {
		probed := sealedProbeTask(t, config, "")
		tamper(t, probed)
		outcome, cleanup, err := probed.td.Collect(probed.source.ref)
		must(t, cleanup)
		if err == nil || outcome != nil {
			t.Fatalf("collect accepted a request that no longer matches its hash: %+v %v", outcome, err)
		}
		if len(probed.source.inputs) != 0 {
			t.Fatal("publication interpreter reached with an unverified request")
		}
		testAbsent(t, filepath.Join(probed.td.Dir, "outcome.json"))
	})
	t.Run("recheck", func(t *testing.T) {
		probed := sealedProbeTask(t, config, "")
		outcome, cleanup, err := probed.td.Collect(probed.source.ref)
		must(t, cleanup)
		must(t, err)
		if outcome == nil || outcome.Verdict != task.VerdictRejected {
			t.Fatalf("source outcome = %+v", outcome)
		}
		tamper(t, probed)
		if _, err = probed.td.Recheck(probed.recovery); err == nil {
			t.Fatal("recheck accepted a request that no longer matches its hash")
		}
		if len(probed.recovery.inputs) != 0 {
			t.Fatal("recovery interpreter reached with an unverified request")
		}
	})
}
