package opencode

import (
	"bytes"
	"fmt"
	"io"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// acpAnswer observes ordered root answer text without retaining it. When a
// sink is attached, decoded text is streamed to it unchanged. A sink write
// failure is kept separately: it is an operational fault of the caller's
// writer, not a property of the native evidence.
type acpAnswer struct {
	sink     io.Writer
	seen     bool
	nonblank bool
	sinkErr  error
	// runs observes root message runs for the acp-v2 final-run selection and
	// decides which text reaches the sink. The live protocol and acp-v1
	// attach none, so every root text chunk is the answer.
	runs *acpMessageRuns
}

// message observes the boundary of one root agent_message_chunk, text or
// not, before its content is read.
func (a *acpAnswer) message(update providerjson.Value) {
	if a.runs != nil {
		a.runs.message(update)
	}
}

func (a *acpAnswer) add(text providerjson.Value) error {
	a.seen = true
	if err := text.CopyString(a); err != nil {
		if a.sinkErr != nil {
			return a.sinkErr
		}
		return fmt.Errorf("%w: answer text: %w", errACPProtocol, err)
	}
	return nil
}

func (a *acpAnswer) Write(p []byte) (int, error) {
	if !a.nonblank && len(bytes.TrimSpace(p)) > 0 {
		a.nonblank = true
	}
	streams := a.sink != nil
	if a.runs != nil {
		a.runs.text(p)
		streams = streams && a.runs.streams()
	}
	if !streams {
		return len(p), nil
	}
	n, err := a.sink.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		a.sinkErr = err
	}
	return n, err
}

// acpUsage is the optional accounting reported by the terminal prompt
// response. Absent counters stay nil; malformed usage is ignored and
// reported as unavailable rather than deciding the turn.
type acpUsage struct {
	present bool
	invalid bool
	metrics task.UsageMetadata
}

var acpUsageCounters = []string{"inputTokens", "outputTokens", "totalTokens", "thoughtTokens", "cachedReadTokens", "cachedWriteTokens"}

func parseACPUsage(value providerjson.Value) acpUsage {
	if raw, err := value.Raw(len("null")); err == nil && string(raw) == "null" {
		return acpUsage{}
	}
	usage := acpUsage{present: true}
	fields, err := value.Object(acpUsageCounters...)
	if err != nil {
		usage.invalid = true
		return usage
	}
	metrics := task.UsageMetadata{Scope: task.UsageScopeUnknown, Source: task.UsageSourceProviderEnvelope}
	targets := []**int64{&metrics.InputTokens, &metrics.OutputTokens, &metrics.TotalTokens, &metrics.ThinkingTokens, &metrics.CacheReadTokens, &metrics.CacheCreationTokens}
	for index, name := range acpUsageCounters {
		counter, present := fields[name]
		if !present {
			continue
		}
		parsed, ok := acpUsageCounter(counter)
		if !ok {
			usage.invalid = true
			return usage
		}
		*targets[index] = parsed
	}
	usage.metrics = metrics
	return usage
}

// acpUsageCounter accepts a nonnegative integer or null.
func acpUsageCounter(value providerjson.Value) (*int64, bool) {
	raw, err := value.Raw(32)
	if err != nil {
		return nil, false
	}
	if string(raw) == "null" {
		return nil, true
	}
	parsed, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || parsed < 0 {
		return nil, false
	}
	return &parsed, true
}

// records returns the usage view with reliability, or none when the native
// response reported no valid usage.
func (u acpUsage) records(reliability string) []task.UsageMetadata {
	if !u.present || u.invalid {
		return nil
	}
	metrics := u.metrics
	metrics.Reliability = reliability
	return []task.UsageMetadata{metrics}
}
