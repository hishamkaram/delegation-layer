package antigravity

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

// currentSuccessorEnvironment is the raw environment B a continuation is
// prepared under: its own HOME and agy, native discovery and cache locations
// that derive writable roots, and values the launch must filter out.
type currentSuccessorEnvironment struct {
	raw            []string
	home           string
	executable     string
	discoveryRoot  string
	cacheRoot      string
	filteredOutKey string
}

func newCurrentSuccessorEnvironment(t *testing.T, workspace string) currentSuccessorEnvironment {
	t.Helper()
	raw, home, executable := selectedAgyEnvironment(t, workspace)
	current := currentSuccessorEnvironment{
		home: home, executable: executable, filteredOutKey: "CURRENT_PROVIDER_SECRET",
		discoveryRoot: filepath.Join(home, "current-xdg-data"), cacheRoot: filepath.Join(home, "current-go-cache"),
	}
	current.raw = append(raw,
		"XDG_DATA_HOME="+current.discoveryRoot, "GOCACHE="+current.cacheRoot,
		current.filteredOutKey+"=redacted", "PUEUE_WORKER=bookkeeping",
	)
	return current
}

// requireCurrentLaunch asserts that candidate launches under the filtered
// values and derived roots of the current environment, resolves its executable
// from it, and carries nothing of the predecessor's saved launch.
func (c currentSuccessorEnvironment) requireCurrentLaunch(t *testing.T, label string, candidate commonprovider.ProfileCandidate, predecessor task.MetaRecord) {
	t.Helper()
	if candidate.Inspection == nil {
		t.Fatalf("%s has no runtime inspection", label)
	}
	c.requireCurrentValues(t, label, candidate.Inspection.Environment, predecessor)
	if candidate.Inspection.Executable != c.executable || candidate.Inspection.ExecutableSHA256 != fixtureSHA256(t, c.executable) {
		t.Errorf("%s executable %q, want the current environment's %q", label, candidate.Inspection.Executable, c.executable)
	}
	c.requireCurrentRoots(t, label, candidate.WritableRoots, predecessor)
}

func (c currentSuccessorEnvironment) requireCurrentValues(t *testing.T, label string, values []string, predecessor task.MetaRecord) {
	t.Helper()
	for _, want := range []string{"HOME=" + c.home, "XDG_DATA_HOME=" + c.discoveryRoot, "GOCACHE=" + c.cacheRoot} {
		if !slices.Contains(values, want) {
			t.Errorf("%s inspection environment lacks %q", label, want)
		}
	}
	for _, entry := range values {
		if strings.HasPrefix(entry, c.filteredOutKey+"=") || strings.HasPrefix(entry, "PUEUE_") {
			t.Errorf("%s inspection environment kept unfiltered %q", label, strings.SplitN(entry, "=", 2)[0])
		}
	}
	for _, saved := range predecessor.Environment {
		key, _, _ := strings.Cut(saved, "=")
		if current := environmentValue(c.raw, key); current != "" && !slices.Contains(c.raw, saved) && slices.Contains(values, saved) {
			t.Errorf("%s inspection environment kept the predecessor value of %q over the current %q", label, key, current)
		}
	}
}

func (c currentSuccessorEnvironment) requireCurrentRoots(t *testing.T, label string, roots []string, predecessor task.MetaRecord) {
	t.Helper()
	for _, want := range []string{c.discoveryRoot, c.cacheRoot} {
		if !slices.Contains(roots, want) {
			t.Errorf("%s writable roots %q lack the current %q", label, roots, want)
		}
	}
	predecessorHome := environmentValue(predecessor.Environment, "HOME")
	if predecessorHome == "" {
		t.Fatalf("%s: predecessor fixture has no recorded HOME to tell its roots from the current ones", label)
	}
	for _, root := range predecessor.EffectiveConfig.Policy.WritableRoots {
		if strings.HasPrefix(root, predecessorHome+string(filepath.Separator)) && slices.Contains(roots, root) {
			t.Errorf("%s writable roots kept the predecessor root %q", label, root)
		}
	}
}

func environmentValue(environment []string, key string) string {
	for _, entry := range environment {
		if value, found := strings.CutPrefix(entry, key+"="); found {
			return value
		}
	}
	return ""
}

func definitionDigest(t *testing.T, candidate commonprovider.ProfileCandidate) string {
	t.Helper()
	_, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func cloneMeta(meta task.MetaRecord) task.MetaRecord {
	cloned := meta
	cloned.Environment = slices.Clone(meta.Environment)
	if meta.EffectiveConfig.Policy != nil {
		policy := *meta.EffectiveConfig.Policy
		policy.WritableRoots = slices.Clone(policy.WritableRoots)
		policy.Sources = slices.Clone(policy.Sources)
		cloned.EffectiveConfig.Policy = &policy
	}
	return cloned
}

// currentEnvironmentSelectors yields the two ways a caller selects B: an
// explicit list, and the process environment when the process itself is B.
func currentEnvironmentSelectors(current currentSuccessorEnvironment) map[string]func(t *testing.T) []string {
	return map[string]func(t *testing.T) []string{
		"explicit": func(*testing.T) []string { return slices.Clone(current.raw) },
		"default": func(t *testing.T) []string {
			for _, entry := range current.raw {
				key, value, _ := strings.Cut(entry, "=")
				t.Setenv(key, value)
			}
			return os.Environ()
		},
	}
}

func TestSuccessorCandidateLaunchesUnderTheCurrentEnvironment(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		fixture := newSuccessorFixture(t, mode)
		current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
		for selector, environment := range currentEnvironmentSelectors(current) {
			t.Run(mode+"/"+selector, func(t *testing.T) {
				meta := cloneMeta(fixture.meta)
				candidate, err := PrepareSuccessorCandidate(fixture.request, meta, environment(t))
				if err != nil {
					t.Fatal(err)
				}
				current.requireCurrentLaunch(t, "successor", candidate, meta)
				prepared, err := candidate.Admit()
				if err != nil {
					t.Fatal(err)
				}
				requireAdmittedCurrentLaunch(t, current, mode, candidate, prepared, meta)
			})
		}
	}
}

// TestSuccessorCandidateEqualsFreshAdmissionUnderTheCurrentEnvironment pins the
// successor's launch values, roots, executable, and proof to what a new task
// admitted in the same environment gets; only the approval and predicate come
// from the predecessor.
func TestSuccessorCandidateEqualsFreshAdmissionUnderTheCurrentEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
	fresh, err := PrepareCandidate(fixture.request, current.raw)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, current.raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Inspection.Environment, successor.Inspection.Environment) || !slices.Equal(fresh.WritableRoots, successor.WritableRoots) ||
		definitionDigest(t, fresh) != definitionDigest(t, successor) {
		t.Error("successor launch values, roots, or inspection definition differ from a fresh admission in the same environment")
	}
	freshAdmitted, err := fresh.Admit()
	if err != nil {
		t.Fatal(err)
	}
	successorAdmitted, err := successor.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if freshAdmitted.RuntimePreflightSHA256 != successorAdmitted.RuntimePreflightSHA256 || !task.CompareEffectiveConfigs(freshAdmitted.Effective, successorAdmitted.Effective) {
		t.Error("successor runtime proof or effective configuration differ from a fresh admission in the same environment")
	}
}

// TestSuccessorCandidateRoundTripsItsFilteredInspectionEnvironment rebuilds the
// successor from the filtered environment the worker inspects with; the
// prospective inspection definition must not change.
func TestSuccessorCandidateRoundTripsItsFilteredInspectionEnvironment(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		fixture := newSuccessorFixture(t, mode)
		current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
		t.Run(mode, func(t *testing.T) {
			raw, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, current.raw)
			if err != nil {
				t.Fatal(err)
			}
			rebuilt, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, slices.Clone(raw.Inspection.Environment))
			if err != nil {
				t.Fatal(err)
			}
			if definitionDigest(t, raw) != definitionDigest(t, rebuilt) || !slices.Equal(raw.WritableRoots, rebuilt.WritableRoots) {
				t.Error("rebuilding from the filtered inspection environment changed the inspection definition or writable roots")
			}
			rawAdmitted, rawErr := raw.Admit()
			rebuiltAdmitted, rebuiltErr := rebuilt.Admit()
			if rawErr != nil || rebuiltErr != nil || rawAdmitted.RuntimePreflightSHA256 != rebuiltAdmitted.RuntimePreflightSHA256 ||
				!task.CompareEffectiveConfigs(rawAdmitted.Effective, rebuiltAdmitted.Effective) {
				t.Errorf("rebuilt admission differs: errors raw=%v rebuilt=%v", rawErr, rebuiltErr)
			}
		})
	}
}

// TestAdmittedSuccessorReconstructsFromItsOwnRecord keeps reconstruction of an
// admitted continuation on the metadata it saved, whatever environment the
// reconstructing caller holds, and on the same inspection definition.
func TestAdmittedSuccessorReconstructsFromItsOwnRecord(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
	candidate, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, current.raw)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	saved := task.MetaRecord{
		EffectiveConfig: admitted.Effective, RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
		Environment: slices.Clone(admitted.Plan.Environment), EnvironmentRecorded: true, Predicate: admitted.Plan.Predicate,
	}
	other := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
	other.raw = append(other.raw, "XDG_STATE_HOME="+filepath.Join(other.home, "other-state"))
	scope := slices.Clone(other.raw)
	scope = slices.DeleteFunc(scope, func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	scope = append(scope, "PATH="+filepath.Dir(current.executable))
	reconstructed, err := PrepareExistingCandidate(fixture.request, saved, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reconstructed.Inspection.Environment, saved.Environment) || !slices.Equal(reconstructed.WritableRoots, saved.EffectiveConfig.Policy.WritableRoots) {
		t.Error("reconstruction did not use the admitted successor's saved environment and roots")
	}
	if definitionDigest(t, reconstructed) != definitionDigest(t, candidate) {
		t.Error("reconstruction of the admitted successor changed the inspection definition")
	}
}

// TestSuccessorCandidateRejectsAnInvalidCurrentEnvironment keeps the
// predecessor's saved launch from standing in for a missing or invalid current
// one. Reconstruction of the same record still uses its saved environment.
func TestSuccessorCandidateRejectsAnInvalidCurrentEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
	without := func(prefix string) []string {
		return slices.DeleteFunc(slices.Clone(current.raw), func(entry string) bool { return strings.HasPrefix(entry, prefix) })
	}
	cases := map[string]struct {
		environment      []string
		existingContinue bool
	}{
		"nil":             {environment: nil},
		"empty":           {environment: []string{}},
		"no HOME":         {environment: without("HOME="), existingContinue: true},
		"relative HOME":   {environment: append(without("HOME="), "HOME=relative"), existingContinue: true},
		"duplicate key":   {environment: append(slices.Clone(current.raw), "GOCACHE=/duplicate"), existingContinue: true},
		"entry without =": {environment: append(slices.Clone(current.raw), "NOVALUE"), existingContinue: true},
		"no PATH":         {environment: without("PATH=")},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			candidate, err := PrepareSuccessorCandidate(fixture.request, cloneMeta(fixture.meta), test.environment)
			if err == nil || !errors.Is(err, ErrUnsupportedProfile) || candidate.Inspection != nil || candidate.Admit != nil || candidate.Finalize != nil {
				t.Fatalf("successor under an invalid current environment: candidate=%+v err=%v", candidate, err)
			}
			if !test.existingContinue {
				return
			}
			if _, err = PrepareExistingCandidate(fixture.request, cloneMeta(fixture.meta), test.environment); err != nil {
				t.Errorf("reconstruction of the same record no longer uses its saved environment: %v", err)
			}
		})
	}
}

// TestSuccessorCandidateKeepsRecordedEligibility keeps the predecessor's
// approval, output-predicate upgrade allowlist, and native/portable selection
// when the launch comes from the current environment.
func TestSuccessorCandidateKeepsRecordedEligibility(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		fixture := newSuccessorFixture(t, mode)
		current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
		current.requireRecordedEligibility(t, fixture, mode)
	}
}

func (c currentSuccessorEnvironment) requireRecordedEligibility(t *testing.T, fixture successorFixture, mode string) {
	t.Helper()
	upgraded := NewRecordPrintInterpreter(mode).Reference()
	retained := map[string]struct{ recorded, want task.PredicateRef }{
		"base 1.2.2":     {NewPrintInterpreter().Reference(), NewPrintInterpreter().Reference()},
		"current record": {upgraded, upgraded},
		"no predicate":   {task.PredicateRef{}, NewCurrentPrintInterpreterForMode(mode).Reference()},
	}
	for name, test := range retained {
		t.Run(mode+"/retains "+name, func(t *testing.T) {
			meta := cloneMeta(fixture.meta)
			meta.Predicate = test.recorded
			if prepared := c.finalizeSuccessor(t, fixture.request, meta); !prepared.Plan.Predicate.Equal(test.want) {
				t.Errorf("predicate %+v, want %+v", prepared.Plan.Predicate, test.want)
			}
		})
	}
	for _, source := range eligibleSuccessorSources(mode) {
		t.Run(mode+"/upgrades "+source.Version, func(t *testing.T) {
			meta := cloneMeta(fixture.meta)
			meta.Predicate = source
			if prepared := c.finalizeSuccessor(t, fixture.request, meta); !prepared.Plan.Predicate.Equal(upgraded) {
				t.Errorf("predicate %+v, want the current record reference", prepared.Plan.Predicate)
			}
		})
	}
	t.Run(mode+"/refuses an unknown recorded approval", func(t *testing.T) {
		meta := cloneMeta(fixture.meta)
		meta.EffectiveConfig.Approval = "bypass-everything"
		if _, err := PrepareSuccessorCandidate(fixture.request, meta, c.raw); !errors.Is(err, ErrUnsupportedProfile) {
			t.Errorf("unknown recorded approval accepted: %v", err)
		}
	})
}

func (c currentSuccessorEnvironment) finalizeSuccessor(t *testing.T, request task.TaskRecord, meta task.MetaRecord) commonprovider.PreparedProfile {
	t.Helper()
	candidate, err := PrepareSuccessorCandidate(request, meta, c.raw)
	if err != nil {
		t.Fatal(err)
	}
	c.requireCurrentLaunch(t, "successor", candidate, meta)
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable, SHA256: candidate.Inspection.ExecutableSHA256, Version: "fixture",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestSuccessorCandidateRetainsRecordedApprovalUnderTheCurrentEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
	for _, approval := range []string{"accept-edits", unattendedApproval} {
		t.Run(approval, func(t *testing.T) {
			meta := cloneMeta(fixture.meta)
			meta.EffectiveConfig.Approval = approval
			prepared := current.finalizeSuccessor(t, fixture.request, meta)
			skipsPermissions := slices.Contains(prepared.Plan.Arguments, "--dangerously-skip-permissions")
			if prepared.Effective.Approval != approval || skipsPermissions != (approval == unattendedApproval) {
				t.Errorf("approval=%q skip-permissions=%t, recorded %q", prepared.Effective.Approval, skipsPermissions, approval)
			}
		})
	}
}

// TestSuccessorCandidateKeepsStaticAdmissionPolicy keeps the existing rule that
// a record sealed without a runtime preflight is not statically admitted, while
// the launch still comes from the current environment.
func TestSuccessorCandidateKeepsStaticAdmissionPolicy(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
	meta := cloneMeta(fixture.meta)
	meta.RuntimePreflightSHA256 = ""
	meta.Environment, meta.EnvironmentRecorded = nil, false
	for name, prepare := range map[string]commonprovider.PrepareExistingCandidate{"existing": PrepareExistingCandidate, "successor": PrepareSuccessorCandidate} {
		candidate, err := prepare(fixture.request, meta, current.raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if candidate.Admit != nil {
			t.Errorf("%s statically admitted a record without a runtime preflight", name)
		}
	}
	candidate, err := PrepareSuccessorCandidate(fixture.request, meta, current.raw)
	if err != nil {
		t.Fatal(err)
	}
	current.requireCurrentLaunch(t, "successor without a preflight", candidate, fixture.meta)
}

func TestSuccessorCandidateLeavesThePredecessorRecordUntouched(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
	fixture.meta.Predicate = NewCurrentPrintInterpreterForMode(ModeWorkspaceWrite).Reference()
	before := cloneMeta(fixture.meta)
	candidate, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, current.raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = candidate.Admit(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixture.meta) || !fixture.meta.EnvironmentRecorded {
		t.Error("predecessor record, its environment, or its roots changed")
	}
}

// portableCurrentEnvironment is a portable-eligible current environment: its own
// HOME and agy, a temporary root that derives a writable root, and values the
// launch must filter out.
type portableCurrentEnvironment struct {
	raw        []string
	home       string
	executable string
	tempRoot   string
}

func newPortableCurrentEnvironment(t *testing.T, workspace string) portableCurrentEnvironment {
	t.Helper()
	raw, home, executable := selectedAgyEnvironment(t, workspace)
	tempRoot := filepath.Join(home, "current-tmp")
	raw = append(raw, "TMPDIR="+tempRoot, "CURRENT_PROVIDER_SECRET=redacted", "PUEUE_WORKER=bookkeeping")
	return portableCurrentEnvironment{raw: raw, home: home, executable: executable, tempRoot: tempRoot}
}

// requireLaunch asserts that candidate launches the current executable under
// the filtered values and derived roots of the current environment, and keeps
// none of the predecessor's saved launch.
func (c portableCurrentEnvironment) requireLaunch(t *testing.T, label string, candidate commonprovider.ProfileCandidate, predecessor task.MetaRecord) {
	t.Helper()
	predecessorHome := environmentValue(predecessor.Environment, "HOME")
	requireSelectedAgy(t, label, candidate, c.home, c.executable, predecessorHome)
	if candidate.Inspection.ExecutableSHA256 != fixtureSHA256(t, c.executable) || !slices.Contains(candidate.Inspection.Environment, "TMPDIR="+c.tempRoot) {
		t.Errorf("%s did not launch under the current executable and values", label)
	}
	for _, entry := range candidate.Inspection.Environment {
		if strings.HasPrefix(entry, "CURRENT_PROVIDER_SECRET=") || strings.HasPrefix(entry, "PUEUE_") {
			t.Errorf("%s inspection environment kept unfiltered %q", label, strings.SplitN(entry, "=", 2)[0])
		}
	}
	if !slices.Contains(candidate.WritableRoots, c.tempRoot) {
		t.Errorf("%s writable roots %q lack the current %q", label, candidate.WritableRoots, c.tempRoot)
	}
	for _, root := range predecessor.EffectiveConfig.Policy.WritableRoots {
		if strings.HasPrefix(root, predecessorHome+string(filepath.Separator)) && slices.Contains(candidate.WritableRoots, root) {
			t.Errorf("%s writable roots kept the predecessor root %q", label, root)
		}
	}
}

// portableRecord returns meta as the portable profile revision records it.
func portableRecord(meta task.MetaRecord) task.MetaRecord {
	portable := cloneMeta(meta)
	policy := *portable.EffectiveConfig.Policy
	policy.ProfileRevision = ProfileRevision
	portable.EffectiveConfig.Policy = &policy
	return portable
}

// TestSuccessorCandidateForPortableRecordUsesTheCurrentEnvironment pins a
// portable predecessor to the reconstruction path: under a portable-eligible
// current environment both factories launch the current executable, filtered
// values, and roots, and neither carries the predecessor's saved launch.
func TestSuccessorCandidateForPortableRecordUsesTheCurrentEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	meta := portableRecord(fixture.meta)
	current := newPortableCurrentEnvironment(t, fixture.request.CanonicalCwd)
	if environmentValue(meta.Environment, "HOME") == current.home {
		t.Fatalf("predecessor HOME cannot be told from the current %q", current.home)
	}
	successor, err := PrepareSuccessorCandidate(fixture.request, meta, current.raw)
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	existing, err := PrepareExistingCandidate(fixture.request, meta, current.raw)
	if err != nil {
		t.Fatalf("existing: %v", err)
	}
	current.requireLaunch(t, "successor", successor, meta)
	current.requireLaunch(t, "existing", existing, meta)
	if definitionDigest(t, successor) != definitionDigest(t, existing) || !slices.Equal(successor.WritableRoots, existing.WritableRoots) {
		t.Error("portable successor differs from the portable reconstruction")
	}
}

// TestSuccessorCandidateForPortableRecordRefusesAlternateDiscovery keeps the
// portable selector rule: a current environment that selects alternate
// provider discovery is refused identically by both factories.
func TestSuccessorCandidateForPortableRecordRefusesAlternateDiscovery(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	current := newCurrentSuccessorEnvironment(t, fixture.request.CanonicalCwd)
	meta := portableRecord(fixture.meta)
	_, successorErr := PrepareSuccessorCandidate(fixture.request, meta, current.raw)
	_, existingErr := PrepareExistingCandidate(fixture.request, meta, current.raw)
	if !errors.Is(successorErr, ErrUnsupportedProfile) || existingErr == nil || existingErr.Error() != successorErr.Error() {
		t.Fatalf("portable successor error=%v, reconstruction error=%v", successorErr, existingErr)
	}
}

// requireAdmittedCurrentLaunch asserts that the admitted successor carries the
// inspected current launch and its own freshly derived proof, not the
// predecessor's.
func requireAdmittedCurrentLaunch(t *testing.T, current currentSuccessorEnvironment, mode string, candidate commonprovider.ProfileCandidate, prepared commonprovider.PreparedProfile, meta task.MetaRecord) {
	t.Helper()
	if !slices.Equal(prepared.Plan.Environment, candidate.Inspection.Environment) ||
		prepared.Plan.Executable != current.executable || !slices.Equal(prepared.WritableRoots, candidate.WritableRoots) ||
		!slices.Equal(prepared.Effective.Policy.WritableRoots, candidate.WritableRoots) {
		t.Errorf("admitted successor plan environment/executable/roots diverge from the inspected current launch")
	}
	if prepared.RuntimePreflightSHA256 == meta.RuntimePreflightSHA256 || prepared.Effective.Policy.RuntimeSHA256 == meta.EffectiveConfig.Policy.RuntimeSHA256 {
		t.Errorf("admitted successor reused the predecessor's runtime proof")
	}
	if want, err := commonprovider.RuntimeCapabilityDigest(*candidate.Inspection); err != nil || prepared.RuntimePreflightSHA256 != want {
		t.Errorf("runtime preflight %q, want a digest of the current inspection %q (err=%v)", prepared.RuntimePreflightSHA256, want, err)
	}
	if prepared.Effective.Approval != meta.EffectiveConfig.Approval || !prepared.Plan.Predicate.Equal(NewRecordPrintInterpreter(mode).Reference()) {
		t.Errorf("approval %q predicate %+v, want the recorded approval and current record predicate", prepared.Effective.Approval, prepared.Plan.Predicate)
	}
}
