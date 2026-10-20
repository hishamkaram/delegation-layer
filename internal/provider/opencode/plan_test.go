package opencode

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	testRootID  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTaskID  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testSession = "ses_01J9K7P2E5S8V1Z3B2C4D6F8G0"
)

func profileRequest(mode string) task.TaskRecord {
	return task.TaskRecord{
		RootID: testRootID, TaskID: testTaskID, Provider: Provider, Mode: mode,
		CanonicalCwd: "/workspace with spaces", BriefLength: 12, BudgetNanos: int64(120 * time.Second),
		RequestedConfig: task.TaskConfig{Permission: mode, Effort: "default"},
	}
}

func TestRunArgumentsMapCallerModeAndContinuation(t *testing.T) {
	readOnly := profileRequest(ModeReadOnly)
	requireRunArguments(t, readOnly, dialectACP, []string{"acp"})
	requireRunArguments(t, readOnly, dialectNativeV2, []string{"run", "--format", "json", "--standalone", "--agent", nativeReadOnlyAgent})
	requireRunArguments(t, readOnly, dialectNativeV1, []string{"run", "--format", "json", "--dir", readOnly.CanonicalCwd, "--agent", nativeReadOnlyAgent})
	requireRunArguments(t, readOnly, dialectPure, []string{"run", "--format", "json", "--dir", readOnly.CanonicalCwd, "--agent", readOnlyAgentName, "--pure"})

	write := profileRequest(ModeWorkspaceWrite)
	write.RequestedConfig.Model = "anthropic/claude-sonnet"
	write.RequestedConfig.Effort = "high"
	write.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testSession, PredecessorTaskID: testTaskID}
	// ACP carries the agent, model, effort and exact session in protocol
	// requests, never in argv.
	requireRunArguments(t, write, dialectACP, []string{"acp"})
	requireRunArguments(t, write, dialectNativeV2, []string{"run", "--format", "json", "--standalone", "--agent", nativeWorkspaceWriteAgent, "--model", "anthropic/claude-sonnet#high", "--auto", "--session", testSession})
	requireRunArguments(t, write, dialectNativeV1, []string{"run", "--format", "json", "--dir", write.CanonicalCwd, "--agent", nativeWorkspaceWriteAgent, "--model", "anthropic/claude-sonnet", "--variant", "high", "--auto", "--session", testSession})
	requireRunArguments(t, write, dialectPure, []string{"run", "--format", "json", "--dir", write.CanonicalCwd, "--agent", workspaceWriteAgentName, "--pure", "--model", "anthropic/claude-sonnet", "--variant", "high", "--auto", "--session", testSession})
}

func TestCurrentRunArgumentsSelectModelVariant(t *testing.T) {
	cases := map[string]struct {
		model, effort string
		want          []string
	}{
		"no model or effort":            {want: []string{}},
		"default effort keeps no model": {effort: "default", want: []string{}},
		"model without effort":          {model: "anthropic/claude-sonnet", want: []string{"--model", "anthropic/claude-sonnet"}},
		"default effort keeps model":    {model: "anthropic/claude-sonnet", effort: "default", want: []string{"--model", "anthropic/claude-sonnet"}},
		"embedded variant is kept":      {model: "anthropic/claude-sonnet#max", effort: "default", want: []string{"--model", "anthropic/claude-sonnet#max"}},
		"effort joins model":            {model: "openai/gpt-5", effort: "minimal", want: []string{"--model", "openai/gpt-5#minimal"}},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			request := profileRequest(ModeReadOnly)
			request.RequestedConfig.Model, request.RequestedConfig.Effort = testCase.model, testCase.effort
			want := append([]string{"run", "--format", "json", "--standalone", "--agent", nativeReadOnlyAgent}, testCase.want...)
			requireRunArguments(t, request, dialectNativeV2, want)
		})
	}
}

func TestCurrentRunArgumentsRejectUnrepresentableEffort(t *testing.T) {
	cases := map[string]struct{ model, effort, message string }{
		"effort without model":         {effort: "high", message: "requires an explicit model"},
		"effort with embedded variant": {model: "anthropic/claude-sonnet#max", effort: "high", message: "already names a variant"},
		"effort with separator":        {model: "anthropic/claude-sonnet", effort: "high#max", message: "must not contain"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			request := profileRequest(ModeWorkspaceWrite)
			request.RequestedConfig.Model, request.RequestedConfig.Effort = testCase.model, testCase.effort
			_, err := runArguments(request, dialectNativeV2)
			if !errors.Is(err, ErrUnsupportedProfile) || !strings.Contains(err.Error(), testCase.message) {
				t.Fatalf("error=%v want ErrUnsupportedProfile containing %q", err, testCase.message)
			}
		})
	}
	// The historical native dialect recorded effort as --variant without a
	// model; reconstruction of such a record must keep that argv.
	request := profileRequest(ModeReadOnly)
	request.RequestedConfig.Effort = "high"
	requireRunArguments(t, request, dialectNativeV1, []string{"run", "--format", "json", "--dir", request.CanonicalCwd, "--agent", nativeReadOnlyAgent, "--variant", "high"})
}

func requireRunArguments(t *testing.T, request task.TaskRecord, dialect launchDialect, want []string) {
	t.Helper()
	got, err := runArguments(request, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("dialect %d argv=%q want=%q", dialect, got, want)
	}
}

func TestRuntimeRequirementsMatchEachDialectLaunch(t *testing.T) {
	cases := map[launchDialect][]string{
		dialectNativeV2: {"--format", "--session", "--model", "--agent", "--auto", "--standalone"},
		dialectNativeV1: {"--format", "--dir", "--session", "--model", "--variant", "--agent", "--auto"},
		dialectPure:     {"--format", "--dir", "--session", "--model", "--variant", "--agent", "--pure", "--auto"},
	}
	request := profileRequest(ModeWorkspaceWrite)
	request.RequestedConfig.Model, request.RequestedConfig.Effort = "anthropic/claude-sonnet", "high"
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testSession, PredecessorTaskID: testTaskID}
	for dialect, want := range cases {
		requirements, err := dialect.runtimeRequirements()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(requirements.HelpArgs, []string{"run"}) || !slices.Equal(requirements.RequiredFlags, want) {
			t.Fatalf("dialect %d requirements=%+v want flags %q", dialect, requirements, want)
		}
		arguments, err := runArguments(request, dialect)
		if err != nil {
			t.Fatal(err)
		}
		for _, argument := range arguments {
			if strings.HasPrefix(argument, "--") && !slices.Contains(requirements.RequiredFlags, argument) {
				t.Fatalf("dialect %d launches unprobed flag %q", dialect, argument)
			}
		}
	}
}

func TestACPRuntimeRequirementsInventNoFlag(t *testing.T) {
	request := profileRequest(ModeWorkspaceWrite)
	request.RequestedConfig.Model, request.RequestedConfig.Effort = "anthropic/claude-sonnet", "high"
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testSession, PredecessorTaskID: testTaskID}
	acp, err := dialectACP.runtimeRequirements()
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := runArguments(request, dialectACP)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(acp.HelpArgs, []string{"acp"}) || len(acp.RequiredFlags) != 0 || !slices.Equal(arguments, []string{"acp"}) {
		t.Fatalf("ACP requirements=%+v argv=%q want help [acp], no flags, argv [acp]", acp, arguments)
	}
	if !slices.Equal(Description().Runtime.HelpArgs, acp.HelpArgs) || len(Description().Runtime.RequiredFlags) != 0 {
		t.Fatalf("description runtime=%+v want the current ACP dialect", Description().Runtime)
	}
}

func TestRunArgumentsRejectInvalidRequests(t *testing.T) {
	cases := map[string]func(*task.TaskRecord){
		"provider": func(r *task.TaskRecord) { r.Provider = "other:run" },
		"mode":     func(r *task.TaskRecord) { r.Mode = "unsupported" },
		"permission mismatch": func(r *task.TaskRecord) {
			r.RequestedConfig.Permission = ModeReadOnly
		},
		"native timeout": func(r *task.TaskRecord) { r.RequestedConfig.NativeTimeout = "1s" },
		"empty budget":   func(r *task.TaskRecord) { r.BudgetNanos = 0 },
		"empty brief":    func(r *task.TaskRecord) { r.BriefLength = 0 },
		"relative cwd":   func(r *task.TaskRecord) { r.CanonicalCwd = "relative" },
		"bad model":      func(r *task.TaskRecord) { r.RequestedConfig.Model = "bad\nmodel" },
		"bad effort":     func(r *task.TaskRecord) { r.RequestedConfig.Effort = "bad\teffort" },
		"bad continuation": func(r *task.TaskRecord) {
			r.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: "latest", PredecessorTaskID: testTaskID}
		},
	}
	for name, mutate := range cases {
		for _, dialect := range []launchDialect{dialectACP, dialectNativeV2} {
			t.Run(fmt.Sprintf("%s/%d", name, dialect), func(t *testing.T) {
				request := profileRequest(ModeWorkspaceWrite)
				mutate(&request)
				if _, err := runArguments(request, dialect); !errors.Is(err, ErrUnsupportedProfile) {
					t.Fatalf("error=%v want unsupported profile", err)
				}
			})
		}
	}
}

func TestDescriptionAdvertisesDirectRuntimeCapabilities(t *testing.T) {
	description := Description()
	if description.ID != Provider || !description.Discoverable {
		t.Fatalf("description=%+v", description)
	}
	if !slices.Equal(description.SupportedModes, []string{ModeReadOnly, ModeWorkspaceWrite}) {
		t.Fatalf("modes=%q", description.SupportedModes)
	}
	if len(description.Runtime.RequiredFlags) != 0 || !slices.Equal(description.Runtime.HelpArgs, []string{"acp"}) {
		t.Fatalf("runtime=%+v", description.Runtime)
	}
	if slices.Contains(description.SupportedOptions, commonprovider.OptionInteractive) {
		t.Fatal("OpenCode advertises interactive tasks without a proven native question protocol")
	}
	assertOpenCodeRegistration(t, Registration())
}

// assertOpenCodeRegistration requires every ACP and run reference, for both
// modes, to be registered exactly once and in that order.
func assertOpenCodeRegistration(t *testing.T, registration commonprovider.Registration) {
	t.Helper()
	var want []task.PredicateRef
	for _, references := range []func(string) task.PredicateRef{
		ReferenceForMode, runReferenceForMode, historicalCurrentV4ReferenceForMode, historicalCurrentReferenceForMode, historicalReferenceForMode, LegacyReferenceForMode,
	} {
		want = append(want, references(ModeReadOnly), references(ModeWorkspaceWrite))
	}
	if registration.Prepare == nil || registration.PrepareExisting == nil || registration.PrepareSuccessor == nil || len(registration.Interpreters) != len(want) {
		t.Fatalf("registration has %d interpreters, want %d", len(registration.Interpreters), len(want))
	}
	seen := map[task.PredicateRef]bool{}
	for index, interpreter := range registration.Interpreters {
		reference := interpreter.Reference()
		if !reference.Equal(want[index]) || seen[reference] {
			t.Fatalf("interpreter %d reference=%+v want %+v exactly once", index, reference, want[index])
		}
		seen[reference] = true
	}
	if !registration.Interpreters[0].Reference().Equal(acpReferenceForMode(ModeReadOnly)) || ReferenceForMode(ModeWorkspaceWrite).Version != acpPredicateVersion {
		t.Fatal("the current references are not the ACP predicate")
	}
}
