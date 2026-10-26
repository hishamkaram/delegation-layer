package pi

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// piScope is one caller-selected launch scope: its own HOME, TMPDIR, and a PATH
// holding a pi fixture whose bytes differ from every other scope's. roots lists
// the directories that must never appear in another scope's launch.
type piScope struct {
	environment []string
	home        string
	cli         string
	roots       []string
}

// newPiScope builds a scope. selectors names the native directory selectors the
// scope sets explicitly (piAgentDirEnv, piSessionDirEnv); each points at a
// directory under the scope's HOME.
func newPiScope(t *testing.T, label string, selectors ...string) piScope {
	t.Helper()
	canonical := func() string {
		directory, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return directory
	}
	home, cliDirectory, tmp := canonical(), canonical(), canonical()
	cli := filepath.Join(cliDirectory, "pi")
	if err := os.WriteFile(cli, []byte("pi fixture "+label), 0o700); err != nil {
		t.Fatal(err)
	}
	environment := []string{"HOME=" + home, "PATH=" + cliDirectory, "TMPDIR=" + tmp}
	for _, key := range selectors {
		directory := filepath.Join(home, strings.ToLower(key))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		environment = append(environment, key+"="+directory)
	}
	return piScope{environment: environment, home: home, cli: cli, roots: []string{home, cliDirectory, tmp}}
}

func (s piScope) value(key string) string {
	for _, entry := range s.environment {
		if value, found := strings.CutPrefix(entry, key+"="); found {
			return value
		}
	}
	return ""
}

func inspectionDigest(t *testing.T, candidate commonprovider.ProfileCandidate) string {
	t.Helper()
	if candidate.Inspection == nil {
		t.Fatal("candidate has no runtime inspection")
	}
	_, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// launchStrings lists every launch value a candidate and its finalized profile
// expose: inspection and runtime environments, executable paths, arguments, and
// writable roots.
func launchStrings(candidate commonprovider.ProfileCandidate, profile commonprovider.PreparedProfile) []string {
	values := slices.Clone(candidate.Inspection.Environment)
	values = append(values, candidate.Inspection.Runtime.Environment...)
	values = append(values, candidate.Inspection.Executable, profile.Plan.Executable)
	values = append(values, candidate.WritableRoots...)
	values = append(values, profile.WritableRoots...)
	values = append(values, profile.Plan.Environment...)
	return append(values, profile.Plan.Arguments...)
}

func requireNoScope(t *testing.T, label string, foreign piScope, candidate commonprovider.ProfileCandidate, profile commonprovider.PreparedProfile) {
	t.Helper()
	for _, value := range launchStrings(candidate, profile) {
		for _, root := range foreign.roots {
			if strings.Contains(value, root) {
				t.Fatalf("%s carries the predecessor scope %q in %q", label, root, value)
			}
		}
	}
}

func requireSamePreparation(t *testing.T, label string, got, want commonprovider.ProfileCandidate, now time.Time) {
	t.Helper()
	if inspectionDigest(t, got) != inspectionDigest(t, want) {
		t.Fatalf("%s inspection definition differs: %+v want %+v", label, got.Inspection, want.Inspection)
	}
	if !slices.Equal(got.WritableRoots, want.WritableRoots) {
		t.Fatalf("%s writable roots differ: %q want %q", label, got.WritableRoots, want.WritableRoots)
	}
	gotProfile, wantProfile := finalizeProfile(t, got, now), finalizeProfile(t, want, now)
	if !samePlan(gotProfile, wantProfile) {
		t.Fatalf("%s plan differs:\n got %+v\nwant %+v", label, gotProfile.Plan, wantProfile.Plan)
	}
	if !task.CompareEffectiveConfigs(gotProfile.Effective, wantProfile.Effective) || !slices.Equal(gotProfile.WritableRoots, wantProfile.WritableRoots) {
		t.Fatalf("%s policy differs:\n got %+v roots %q\nwant %+v roots %q", label, gotProfile.Effective.Policy, gotProfile.WritableRoots, wantProfile.Effective.Policy, wantProfile.WritableRoots)
	}
}

// requireSameStaticAdmission requires the same eligibility for static admission
// and, when both are eligible, the same admitted profile and capability proof.
func requireSameStaticAdmission(t *testing.T, label string, got, want commonprovider.ProfileCandidate) {
	t.Helper()
	if (got.Admit == nil) != (want.Admit == nil) {
		t.Fatalf("%s static admission present=%t want %t", label, got.Admit != nil, want.Admit != nil)
	}
	if got.Admit == nil {
		return
	}
	gotStatic, err := got.Admit()
	if err != nil {
		t.Fatal(err)
	}
	wantStatic, err := want.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if gotStatic.RuntimePreflightSHA256 != wantStatic.RuntimePreflightSHA256 || !samePlan(gotStatic, wantStatic) {
		t.Fatalf("%s static admission differs: %+v want %+v", label, gotStatic, wantStatic)
	}
}

// samePlan compares two profiles' launch plans; an empty input list equals an
// absent one.
func samePlan(got, want commonprovider.PreparedProfile) bool {
	gotPlan, wantPlan := got.Plan, want.Plan
	if len(gotPlan.InputFiles) == 0 {
		gotPlan.InputFiles = nil
	}
	if len(wantPlan.InputFiles) == 0 {
		wantPlan.InputFiles = nil
	}
	return reflect.DeepEqual(gotPlan, wantPlan)
}

// admittedPredecessor records the current admission of a task launched in
// scope as the predecessor's immutable metadata.
func admittedPredecessor(t *testing.T, request task.TaskRecord, scope piScope, now time.Time) task.MetaRecord {
	t.Helper()
	candidate, err := PrepareCandidate(request, scope.environment)
	if err != nil {
		t.Fatal(err)
	}
	admitted := finalizeProfile(t, candidate, now)
	return task.MetaRecord{
		Predicate:           admitted.Plan.Predicate,
		EffectiveConfig:     admitted.Effective,
		Environment:         slices.Clone(admitted.Plan.Environment),
		EnvironmentRecorded: true,
	}
}

func cloneMeta(meta task.MetaRecord) task.MetaRecord {
	meta.Environment = slices.Clone(meta.Environment)
	return meta
}

func successorRequest(request task.TaskRecord) task.TaskRecord {
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testUUID, PredecessorTaskID: "abcdef0123456789abcdef0123456789"}
	return request
}

func prepareSuccessorOf(t *testing.T, request task.TaskRecord, meta task.MetaRecord, environment []string) commonprovider.ProfileCandidate {
	t.Helper()
	catalog, err := commonprovider.NewCatalog(Registration())
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := catalog.SuccessorCandidate(request, meta, environment)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

// A new successor launches under the caller's environment. The predecessor's
// record selects only compatibility; it neither supplies launch values and
// roots nor keeps an earlier session-directory override alive.
func TestSuccessorLaunchesFromCallerEnvironmentNotPredecessorRecord(t *testing.T) {
	selectorCases := map[string]struct{ predecessor, caller []string }{
		"predecessor overrode both selectors":  {predecessor: []string{piAgentDirEnv, piSessionDirEnv}},
		"predecessor overrode session only":    {predecessor: []string{piSessionDirEnv}},
		"caller overrides both selectors":      {caller: []string{piAgentDirEnv, piSessionDirEnv}},
		"both override different directories":  {predecessor: []string{piSessionDirEnv}, caller: []string{piSessionDirEnv}},
		"neither overrides a native selector":  {},
		"caller overrides the agent directory": {caller: []string{piAgentDirEnv}},
	}
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		for name, selectors := range selectorCases {
			t.Run(mode+"/"+name, func(t *testing.T) {
				request, now := profileReconstructionFixture(t, mode)
				predecessorScope := newPiScope(t, "A", selectors.predecessor...)
				callerScope := newPiScope(t, "B", selectors.caller...)
				meta := admittedPredecessor(t, request, predecessorScope, now)
				recorded := cloneMeta(meta)
				successor := successorRequest(request)

				candidate := prepareSuccessorOf(t, successor, meta, callerScope.environment)
				fresh, err := PrepareCandidate(successor, callerScope.environment)
				if err != nil {
					t.Fatal(err)
				}
				requireSamePreparation(t, "successor", candidate, fresh, now)
				if candidate.Inspection.Executable != callerScope.cli {
					t.Fatalf("successor executable=%q want the caller's %q", candidate.Inspection.Executable, callerScope.cli)
				}
				prepared := finalizeProfile(t, candidate, now)
				requireNoScope(t, "successor", predecessorScope, candidate, prepared)
				requireCallerSessionSelector(t, mode, callerScope, prepared)
				if !reflect.DeepEqual(meta, recorded) {
					t.Fatalf("predecessor metadata changed: %+v want %+v", meta, recorded)
				}
				requireReconstructionUsesRecord(t, request, meta, callerScope, now)
			})
		}
	}
}

// requireCallerSessionSelector requires the caller's explicit session
// directory, when it set one, to be the successor's, and workspace-write to
// keep passing no --session-dir.
func requireCallerSessionSelector(t *testing.T, mode string, caller piScope, prepared commonprovider.PreparedProfile) {
	t.Helper()
	if want := caller.value(piSessionDirEnv); want != "" {
		if !slices.Contains(prepared.Plan.Environment, piSessionDirEnv+"="+want) {
			t.Fatalf("successor ignored the caller's session directory %q: %q", want, prepared.Plan.Environment)
		}
		if mode == ModeReadOnly && !slices.Contains(prepared.Plan.Arguments, want) {
			t.Fatalf("read-only successor arguments=%q want --session-dir %q", prepared.Plan.Arguments, want)
		}
	}
	if mode == ModeWorkspaceWrite && slices.Contains(prepared.Plan.Arguments, "--session-dir") {
		t.Fatalf("workspace-write successor passed --session-dir: %q", prepared.Plan.Arguments)
	}
}

// requireReconstructionUsesRecord requires reconstruction of the predecessor's
// own admitted task to read its saved launch environment, not the caller's.
func requireReconstructionUsesRecord(t *testing.T, request task.TaskRecord, meta task.MetaRecord, caller piScope, now time.Time) {
	t.Helper()
	reconstructed, err := PrepareExistingCandidate(request, meta, caller.environment)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt := finalizeProfile(t, reconstructed, now); !slices.Equal(rebuilt.Plan.Environment, meta.Environment) {
		t.Fatalf("reconstruction environment=%q want the record's %q", rebuilt.Plan.Environment, meta.Environment)
	}
}

// Reconstructing an admitted record, including a successor's own, still reads
// that record's saved launch environment and ignores the caller's.
func TestAdmittedSuccessorReconstructsFromItsOwnSavedMetadata(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			request, now := profileReconstructionFixture(t, mode)
			predecessorScope := newPiScope(t, "A", piSessionDirEnv)
			callerScope := newPiScope(t, "B")
			laterScope := newPiScope(t, "C", piAgentDirEnv)
			meta := admittedPredecessor(t, request, predecessorScope, now)
			successor := successorRequest(request)
			candidate := prepareSuccessorOf(t, successor, meta, callerScope.environment)
			admitted := finalizeProfile(t, candidate, now)
			saved := task.MetaRecord{
				Predicate:           admitted.Plan.Predicate,
				EffectiveConfig:     admitted.Effective,
				Environment:         slices.Clone(admitted.Plan.Environment),
				EnvironmentRecorded: true,
			}

			rebuilt, err := PrepareExistingCandidate(successor, saved, laterScope.environment)
			if err != nil {
				t.Fatal(err)
			}
			finalized := finalizeProfile(t, rebuilt, now)
			if !slices.Equal(finalized.Plan.Environment, admitted.Plan.Environment) || !slices.Equal(finalized.Plan.Arguments, admitted.Plan.Arguments) || !slices.Equal(finalized.WritableRoots, admitted.WritableRoots) {
				t.Fatalf("reconstruction did not reuse the record's saved launch: %+v want %+v", finalized.Plan, admitted.Plan)
			}
			if !slices.Contains(rebuilt.Inspection.Environment, "HOME="+callerScope.home) {
				t.Fatalf("reconstruction inspection environment=%q want the record's HOME", rebuilt.Inspection.Environment)
			}
			if rebuilt.Inspection.Executable != laterScope.cli {
				t.Fatalf("reconstruction executable=%q want the later caller's %q", rebuilt.Inspection.Executable, laterScope.cli)
			}
		})
	}
}

// Preparing a successor from the raw caller environment and again from the
// filtered environment it produced yields the same inspection definition, so a
// worker rebuilding from the persisted launch cannot diverge from admission.
func TestSuccessorFilteredEnvironmentRoundTripsInspectionDefinition(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		for name, selectors := range map[string][]string{
			"default selectors":   nil,
			"explicit selectors":  {piAgentDirEnv, piSessionDirEnv},
			"explicit session":    {piSessionDirEnv},
			"explicit agent only": {piAgentDirEnv},
		} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				request, now := profileReconstructionFixture(t, mode)
				predecessorScope := newPiScope(t, "A", piSessionDirEnv)
				callerScope := newPiScope(t, "B", selectors...)
				meta := admittedPredecessor(t, request, predecessorScope, now)
				successor := successorRequest(request)

				first := prepareSuccessorOf(t, successor, meta, callerScope.environment)
				filtered := slices.Clone(first.Inspection.Environment)
				second := prepareSuccessorOf(t, successor, meta, filtered)
				if inspectionDigest(t, first) != inspectionDigest(t, second) {
					t.Fatalf("filtered rebuild changed the inspection definition:\n raw %+v\nfiltered %+v", first.Inspection, second.Inspection)
				}
				admitted := finalizeProfile(t, first, now)
				saved := task.MetaRecord{
					Predicate:           admitted.Plan.Predicate,
					EffectiveConfig:     admitted.Effective,
					Environment:         slices.Clone(admitted.Plan.Environment),
					EnvironmentRecorded: true,
				}
				worker, err := PrepareExistingCandidate(successor, saved, callerScope.environment)
				if err != nil {
					t.Fatal(err)
				}
				if inspectionDigest(t, worker) != inspectionDigest(t, first) {
					t.Fatalf("admitted-record reconstruction changed the inspection definition:\n admitted %+v\n rebuilt %+v", first.Inspection, worker.Inspection)
				}
			})
		}
	}
}

// An invalid caller environment fails the successor. It never falls back to the
// predecessor's saved environment, which the same record still serves for
// reconstruction of that admitted task.
func TestSuccessorRejectsInvalidCallerEnvironmentWithoutFallback(t *testing.T) {
	request, now := profileReconstructionFixture(t, ModeReadOnly)
	predecessorScope := newPiScope(t, "A")
	callerScope := newPiScope(t, "B")
	meta := admittedPredecessor(t, request, predecessorScope, now)
	successor := successorRequest(request)
	catalog, err := commonprovider.NewCatalog(Registration())
	if err != nil {
		t.Fatal(err)
	}
	without := func(prefix string) []string {
		return slices.DeleteFunc(slices.Clone(callerScope.environment), func(entry string) bool { return strings.HasPrefix(entry, prefix) })
	}
	inside := filepath.Join(request.CanonicalCwd, "agent")
	for name, invalid := range map[string]struct {
		environment     []string
		lacksExecutable bool
	}{
		"malformed entry":                  {environment: append(slices.Clone(callerScope.environment), "not-an-assignment")},
		"duplicate key":                    {environment: append(slices.Clone(callerScope.environment), "HOME="+callerScope.home)},
		"missing HOME":                     {environment: without("HOME=")},
		"relative HOME":                    {environment: append(without("HOME="), "HOME=relative")},
		"agent directory in the workspace": {environment: append(slices.Clone(callerScope.environment), piAgentDirEnv+"="+inside)},
		"session directory is a file":      {environment: append(slices.Clone(callerScope.environment), piSessionDirEnv+"="+callerScope.cli)},
		"missing PATH":                     {environment: without("PATH="), lacksExecutable: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, successorErr := catalog.SuccessorCandidate(successor, meta, invalid.environment); successorErr == nil {
				t.Fatal("successor fell back to the predecessor's saved environment")
			} else if !invalid.lacksExecutable && !errors.Is(successorErr, ErrUnsupportedProfile) {
				t.Fatalf("successor error=%v want ErrUnsupportedProfile", successorErr)
			}
			// The admitted record is unaffected by an invalid caller launch value
			// unless the caller cannot supply the executable.
			_, existingErr := catalog.ExistingCandidate(successor, meta, invalid.environment)
			if invalid.lacksExecutable != (existingErr != nil) {
				t.Fatalf("existing reconstruction error=%v with invalid caller environment %q", existingErr, name)
			}
		})
	}
}

// A saved empty environment is a recorded value for reconstruction, which
// refuses it. A successor never reads the predecessor's environment at all.
func TestSuccessorIgnoresEmptyRecordedPredecessorEnvironment(t *testing.T) {
	fixture := newPiSuccessorFixture(t, ModeReadOnly)
	callerScope := newPiScope(t, "B")
	for name, recorded := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			meta := fixture.meta(priorReferenceForMode(fixture.request.Mode))
			meta.Environment, meta.EnvironmentRecorded = recorded, true
			if _, err := PrepareExistingCandidate(fixture.request, meta, callerScope.environment); err == nil {
				t.Fatal("reconstruction fell back to the provided environment")
			}
			candidate := prepareSuccessorOf(t, fixture.request, meta, callerScope.environment)
			if candidate.Inspection.Executable != callerScope.cli || !slices.Contains(candidate.Inspection.Environment, "HOME="+callerScope.home) {
				t.Fatalf("successor inspection=%+v want the caller's scope", candidate.Inspection)
			}
			unrecorded := meta
			unrecorded.Environment, unrecorded.EnvironmentRecorded = nil, false
			expected := prepareSuccessorOf(t, fixture.request, unrecorded, callerScope.environment)
			requireSamePreparation(t, "recorded-empty successor", candidate, expected, fixture.now)
		})
	}
}

type predecessorCase struct {
	name      string
	mode      string
	reference task.PredicateRef
	pending   bool
	legacy    bool
	wantRef   task.PredicateRef
	wantAdmit bool
}

func predecessorCases() []predecessorCase {
	var cases []predecessorCase
	for _, mode := range piOutcomeModes {
		current, prior := ReferenceForMode(mode), priorReferenceForMode(mode)
		cases = append(cases,
			predecessorCase{name: mode + " current", mode: mode, reference: current, wantRef: current},
			predecessorCase{name: mode + " current pending admission", mode: mode, reference: current, pending: true, wantRef: current, wantAdmit: true},
			predecessorCase{name: mode + " prior fresh default upgrades", mode: mode, reference: prior, wantRef: current},
		)
	}
	return append(cases,
		predecessorCase{name: "read-only v5 stays historical", mode: ModeReadOnly, reference: readOnlyV5Reference(), wantRef: readOnlyV5Reference()},
		predecessorCase{name: "read-only v4 stays historical", mode: ModeReadOnly, reference: readOnlyV4Reference(), wantRef: readOnlyV4Reference()},
		predecessorCase{name: "read-only v2 stays historical", mode: ModeReadOnly, reference: readOnlyV2Reference(), wantRef: readOnlyV2Reference()},
		predecessorCase{name: "workspace-write v7 stays historical", mode: ModeWorkspaceWrite, reference: workspaceWriteV7Reference(), wantRef: workspaceWriteV7Reference()},
		predecessorCase{name: "workspace-write v5 stays historical", mode: ModeWorkspaceWrite, reference: workspaceWriteV5Reference(), wantRef: workspaceWriteV5Reference()},
		predecessorCase{name: "workspace-write v3 stays historical", mode: ModeWorkspaceWrite, reference: workspaceWriteV3Reference(), wantRef: workspaceWriteV3Reference()},
		predecessorCase{name: "legacy stays legacy", mode: ModeReadOnly, legacy: true, wantRef: LegacyReference()},
	)
}

// predecessorMeta is the admitted record of a task launched in scope, reduced to
// the compatibility the case names.
func (c predecessorCase) predecessorMeta(t *testing.T, request task.TaskRecord, scope piScope, now time.Time) task.MetaRecord {
	t.Helper()
	meta := admittedPredecessor(t, request, scope, now)
	if c.legacy {
		meta.Predicate = task.PredicateRef{}
		meta.EffectiveConfig = task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: ProfileRevision}}
	} else {
		meta.Predicate = c.reference
	}
	if c.pending {
		meta.RuntimePreflightSHA256 = strings.Repeat("a", 64)
	}
	return meta
}

// requireLaunchContract requires the historical or current argument shape the
// predecessor's profile selects.
func (c predecessorCase) requireLaunchContract(t *testing.T, prepared commonprovider.PreparedProfile) {
	t.Helper()
	currentLaunch := c.wantRef.Equal(ReferenceForMode(c.mode))
	wantTools := c.mode == ModeReadOnly || currentLaunch
	wantSessionDir := c.mode == ModeReadOnly && (currentLaunch || c.legacy)
	if slices.Contains(prepared.Plan.Arguments, "--tools") != wantTools || slices.Contains(prepared.Plan.Arguments, "--session-dir") != wantSessionDir {
		t.Fatalf("successor arguments=%q want tools=%t session-dir=%t", prepared.Plan.Arguments, wantTools, wantSessionDir)
	}
}

// Selecting the caller's environment does not change which profile, predicate,
// runtime requirements, approval, or static admission the predecessor selects.
// Every historical record produces the same preparation as the unrecorded
// reconstruction of the same predicate under the caller's environment.
func TestSuccessorKeepsPredecessorSelectedCompatibilityUnderCallerEnvironment(t *testing.T) {
	catalog, err := commonprovider.NewCatalog(Registration())
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range predecessorCases() {
		t.Run(testCase.name, func(t *testing.T) {
			request, now := profileReconstructionFixture(t, testCase.mode)
			predecessorScope := newPiScope(t, "A", piSessionDirEnv)
			callerScope := newPiScope(t, "B", piSessionDirEnv)
			meta := testCase.predecessorMeta(t, request, predecessorScope, now)
			successor := successorRequest(request)

			candidate := prepareSuccessorOf(t, successor, meta, callerScope.environment)
			prepared := finalizeProfile(t, candidate, now)
			if !prepared.Plan.Predicate.Equal(testCase.wantRef) {
				t.Fatalf("successor predicate=%+v want %+v", prepared.Plan.Predicate, testCase.wantRef)
			}
			if (candidate.Admit != nil) != testCase.wantAdmit {
				t.Fatalf("successor static admission present=%t want %t", candidate.Admit != nil, testCase.wantAdmit)
			}
			testCase.requireLaunchContract(t, prepared)
			requireNoScope(t, "successor", predecessorScope, candidate, prepared)

			// The oracle: the same predecessor selection with no saved
			// environment, so the caller's environment is the launch environment.
			expectedMeta := meta
			expectedMeta.Environment, expectedMeta.EnvironmentRecorded = nil, false
			if testCase.reference.Equal(priorReferenceForMode(testCase.mode)) {
				expectedMeta.Predicate = ReferenceForMode(testCase.mode)
			}
			expected, err := catalog.ExistingCandidate(successor, expectedMeta, callerScope.environment)
			if err != nil {
				t.Fatal(err)
			}
			requireSamePreparation(t, "successor", candidate, expected, now)
			requireSameStaticAdmission(t, "successor", candidate, expected)
			existing, err := catalog.ExistingCandidate(successor, meta, callerScope.environment)
			if err != nil {
				t.Fatal(err)
			}
			if existingProfile := finalizeProfile(t, existing, now); existingProfile.Effective.Approval != prepared.Effective.Approval {
				t.Fatalf("approval changed: successor=%q reconstruction=%q", prepared.Effective.Approval, existingProfile.Effective.Approval)
			}
		})
	}
}

// A pending predecessor's static admission is eligibility; the capability proof
// is derived from the successor's own inspection definition, not copied.
func TestSuccessorDerivesCapabilityProofFromCallerEnvironment(t *testing.T) {
	request, now := profileReconstructionFixture(t, ModeReadOnly)
	predecessorScope := newPiScope(t, "A")
	callerScope := newPiScope(t, "B")
	meta := admittedPredecessor(t, request, predecessorScope, now)
	predecessorCandidate, err := PrepareCandidate(request, predecessorScope.environment)
	if err != nil {
		t.Fatal(err)
	}
	predecessorProof, err := commonprovider.RuntimeCapabilityDigest(*predecessorCandidate.Inspection)
	if err != nil {
		t.Fatal(err)
	}
	meta.RuntimePreflightSHA256 = predecessorProof

	candidate := prepareSuccessorOf(t, successorRequest(request), meta, callerScope.environment)
	if candidate.Admit == nil {
		t.Fatal("pending predecessor lost static admission eligibility")
	}
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	callerProof, err := commonprovider.RuntimeCapabilityDigest(*candidate.Inspection)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.RuntimePreflightSHA256 != callerProof || admitted.RuntimePreflightSHA256 == predecessorProof {
		t.Fatalf("static capability=%q want the caller's %q, not the predecessor's %q", admitted.RuntimePreflightSHA256, callerProof, predecessorProof)
	}
}
