package app

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/ccr"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// recordedMissingSessionHelp lists every runtime flag the six production
// registrations require, so each runtime probe of the fixture CLI succeeds.
const recordedMissingSessionHelp = "Usage: --json --color --sandbox -c --cd --output-last-message --skip-git-repo-check " +
	"--model --auth-mode --print --permission-mode --input-format --output-format --verbose --permission-prompts " +
	"--session-id --resume --resume-route --effort --mode --tools --no-extensions --session-dir --session " +
	"--thinking --add-dir --print-timeout --conversation --dangerously-skip-permissions"

// recordedMissingSessionCLI is a finite caller-visible provider CLI whose
// recorded session no longer exists. It answers the runtime version and help
// probes, then fails its one launch with failure. It uses shell builtins only,
// because the admitted launch scope's PATH holds just the fixture CLI
// directory. Every probe, launch, and inbound protocol request is appended to
// "$0.log".
func recordedMissingSessionCLI(failure string) string {
	return `#!/bin/sh
log="$0.log"
last=
for arg in "$@"; do last=$arg; done
case "${1-}" in
  version|--version) printf 'probe %s\n' "$*" >> "$log"; printf '%s\n' '1.0.0'; exit 0 ;;
esac
if [ "$last" = --help ]; then
  printf 'probe %s\n' "$*" >> "$log"
  printf '%s\n' '` + recordedMissingSessionHelp + `'
  exit 0
fi
printf 'launch %s\n' "$*" >> "$log"
` + failure
}

// missingSessionFailure is a representative fixture refusal of a resumed
// session the provider cannot load, not captured native output. OpenCode ACP
// acknowledges initialize and answers session/resume with a session-not-found
// error, then records any further request until its input closes; the other
// CLIs report the missing session on stderr and exit nonzero without emitting
// a turn.
func missingSessionFailure(provider string) string {
	switch provider {
	case openCodeProvider:
		return `frame() {
  case "$1" in
    *'"method":"initialize"'*) printf 'frame initialize\n' >> "$log" ;;
    *'"method":"session/resume"'*) printf 'frame session/resume\n' >> "$log"; printf 'resume %s\n' "$1" >> "$log" ;;
    *'"method":"session/new"'*) printf 'frame session/new\n' >> "$log" ;;
    *'"method":"session/load"'*) printf 'frame session/load\n' >> "$log" ;;
    *'"method":"session/prompt"'*) printf 'frame session/prompt\n' >> "$log" ;;
    *) printf 'frame other\n' >> "$log" ;;
  esac
}
IFS= read -r line || exit 1
frame "$line"
printf '%s\n' '` + openCodeACPInitialize + `'
IFS= read -r line || exit 1
frame "$line"
printf '%s\n' '{"jsonrpc":"2.0","id":2,"error":{"code":-32002,"message":"session not found"}}'
while IFS= read -r line; do frame "$line"; done
exit 1
`
	case "codex:exec":
		return "printf '%s\\n' 'Error: thread/resume failed: no rollout found for thread id' >&2\nexit 1\n"
	case "claude:print", "ccr:print":
		return "printf '%s\\n' 'No conversation found with session ID' >&2\nexit 1\n"
	default:
		return "printf '%s\\n' 'Error: session not found' >&2\nexit 1\n"
	}
}

// TestContinuationMissingNativeSessionFailsClosedAtLaunch runs, for every
// production continuation registration and both public commands, a successor
// whose valid recorded predecessor session the provider can no longer load.
// The public command admits the successor in the caller's scope; the runner's
// own profile preparation and launch boundary then start the provider exactly
// once. The provider's refusal seals a rejected outcome with no answer: no
// fresh session, route fallback, retry, or second submission follows, and a
// runner replay under a third scope launches nothing.
func TestContinuationMissingNativeSessionFailsClosedAtLaunch(t *testing.T) {
	for _, testCase := range recordedEnvironmentCases() {
		for _, entryPoint := range []string{"continue --task", "dispatch --resume-task"} {
			t.Run(testCase.name+"/"+entryPoint, func(t *testing.T) {
				runMissingSessionScenario(t, testCase, entryPoint)
			})
		}
	}
}

func runMissingSessionScenario(t *testing.T, testCase recordedEnvironmentCase, entryPoint string) {
	t.Helper()
	fixture := newRecordedEnvironmentFixtureWith(t, testCase, recordedFixtureOptions{
		providerScript:  recordedMissingSessionCLI(missingSessionFailure(testCase.provider)),
		installedRunner: true,
	})
	code, response := fixture.run(t, entryPoint, fixture.supervisor.Binding().ConfigPath)
	if code != 0 || response.TaskRecord != "created" || response.TaskID == "" {
		t.Fatalf("%s exit=%d record=%s failure=%+v", entryPoint, code, response.TaskRecord, response.Failure)
	}
	successor := readImmutableRecords(t, fixture.taskDir(response.TaskID))
	result := fixture.runInstalledRunner(t, response.TaskID)
	if result.Starts != 1 {
		t.Fatalf("provider started %d times, want exactly once (runner code=%d err=%q)", result.Starts, result.Code, result.Err)
	}
	wantCode, wantErr := missingSessionRunnerExit(testCase.provider)
	if result.Code != wantCode || result.Err != wantErr || result.Verdict != task.VerdictRejected || result.Publication != task.PublicationRejected.String() {
		t.Fatalf("missing session was not sealed as a rejected turn: code=%d err=%q verdict=%s publication=%s", result.Code, result.Err, result.Verdict, result.Publication)
	}
	fixture.assertMissingSessionTranscript(t)
	fixture.assertReplayLaunchesNothing(t, response.TaskID)
	fixture.assertPredecessorUnchanged(t)
	if got := readImmutableRecords(t, fixture.taskDir(response.TaskID)); !maps.EqualFunc(got, successor, bytes.Equal) {
		t.Fatal("successor records changed after its rejected launch")
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
}

// missingSessionRunnerExit is the runner's own exit for the sealed rejected
// turn. OpenCode ACP also returns the protocol's refusal of session/resume as
// the runner error; the other providers' refusals are outcome evidence only.
func missingSessionRunnerExit(provider string) (int, string) {
	if provider == openCodeProvider {
		return 1, "OpenCode ACP turn did not end successfully: session/resume request 2 returned JSON-RPC error -32002"
	}
	return 0, ""
}

// assertReplayLaunchesNothing replays the runner for the sealed successor
// under the third, ambient scope. The terminal task returns without starting
// the provider again; the reaped runner left one started, sealed, rejected turn.
func (f *recordedEnvironmentFixture) assertReplayLaunchesNothing(t *testing.T, id string) {
	t.Helper()
	f.ambient.apply(t, f.cliDir)
	var launches launchCounter
	deps := f.deps
	deps.ExecutionHooks = launches.hooks()
	replay := runRunner(runnerArguments{Root: f.root, TaskID: id}, deps)
	if replay.err != nil || replay.code != 0 || launches.starts != 0 {
		t.Fatalf("runner replay under a third scope: code=%d err=%v starts=%d", replay.code, replay.err, launches.starts)
	}
	inspection := f.inspectTask(t, id)
	if !inspection.StartedExists || !inspection.SealExists || !inspection.OutcomeExists || inspection.Publication != task.PublicationRejected {
		t.Fatalf("successor evidence after replay: %+v", inspection)
	}
	f.assertMissingSessionTranscript(t)
}

// assertMissingSessionTranscript requires the fixture CLI's own log to show
// exactly one launch, resuming the predecessor's exact session over CCR's
// recorded route, and for OpenCode ACP only the initialize and session/resume
// requests.
func (f *recordedEnvironmentFixture) assertMissingSessionTranscript(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(f.providerLog())
	if err != nil {
		t.Fatal(err)
	}
	var launches, frames, resumes []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if arguments, ok := strings.CutPrefix(line, "launch "); ok {
			launches = append(launches, arguments)
		}
		if method, ok := strings.CutPrefix(line, "frame "); ok {
			frames = append(frames, method)
		}
		if request, ok := strings.CutPrefix(line, "resume "); ok {
			resumes = append(resumes, request)
		}
	}
	if len(launches) != 1 {
		t.Fatalf("provider CLI launched %d times, want exactly once", len(launches))
	}
	if f.testCase.provider == openCodeProvider {
		if !slices.Equal(frames, openCodeACPResumedTurn[:2]) {
			t.Fatalf("OpenCode ACP requests = %q, want only initialize and session/resume", frames)
		}
		if len(resumes) != 1 || !strings.Contains(resumes[0], `"sessionId":"`+f.session+`"`) {
			t.Fatal("OpenCode ACP session/resume does not name the predecessor's exact session")
		}
		return
	}
	arguments := strings.Fields(launches[0])
	if !slices.Contains(arguments, f.session) {
		t.Fatal("provider launch does not resume the predecessor's exact session")
	}
	if f.testCase.provider == ccr.Provider {
		assertOutputUpgradeCCRLaunch(t, true, arguments)
	}
}

func (f *recordedEnvironmentFixture) providerLog() string {
	return filepath.Join(f.cliDir, f.testCase.binary+".log")
}

// recordedCCRCatalog extends the static CCR substitution to the runner's
// preflight. That preflight re-runs the substitution's runtime-only probe,
// which observes the version and launch help but not the native launch-help
// projection a supervised inspection would record, so the registration's own
// Finalize is given the same recorded route projection the admission used.
// Facts that already carry a native projection pass through unchanged.
func recordedCCRCatalog(t *testing.T) commonprovider.Catalog {
	t.Helper()
	registration, err := outputUpgradeCCRCatalog(t, true).Lookup(ccr.Provider)
	if err != nil {
		t.Fatal(err)
	}
	registration.PrepareExisting = withRecordedCCRRoute(t, registration.PrepareExisting)
	if registration.PrepareSuccessor != nil {
		registration.PrepareSuccessor = withRecordedCCRRoute(t, registration.PrepareSuccessor)
	}
	catalog, err := commonprovider.NewCatalog(registration)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func withRecordedCCRRoute(t *testing.T, prepare commonprovider.PrepareExistingCandidate) commonprovider.PrepareExistingCandidate {
	t.Helper()
	route, err := task.MarshalCanonical(map[string]bool{"resume_route": true})
	if err != nil {
		t.Fatal(err)
	}
	return func(request task.TaskRecord, meta task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, error) {
		candidate, err := prepare(request, meta, environment)
		if err != nil {
			return candidate, err
		}
		finalize := candidate.Finalize
		candidate.Finalize = func(facts json.RawMessage, now time.Time) (commonprovider.PreparedProfile, error) {
			decoded, decodeErr := commonprovider.DecodeInspectionFacts(facts)
			if decodeErr != nil || decoded.Runtime == nil || decoded.Native != nil {
				return finalize(facts, now)
			}
			merged, encodeErr := commonprovider.EncodeInspectionFacts(*decoded.Runtime, route)
			if encodeErr != nil {
				return commonprovider.PreparedProfile{}, encodeErr
			}
			return finalize(merged, now)
		}
		return candidate, nil
	}
}
