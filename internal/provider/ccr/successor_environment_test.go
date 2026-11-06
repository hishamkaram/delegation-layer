package ccr

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ccrCaller is one complete caller-selected environment below its own
// canonical root: a HOME, a TMPDIR, and a PATH holding a fixture ccr whose
// content, and so whose fingerprint, differs from every other caller's.
type ccrCaller struct {
	root, home, temporary, cli, routerRoot string
	values                                 []string
}

func newCCRCaller(t *testing.T, label string) ccrCaller {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caller := ccrCaller{root: root, home: filepath.Join(root, "home"), temporary: filepath.Join(root, "tmp"), cli: filepath.Join(root, "bin", "ccr")}
	caller.routerRoot = filepath.Join(caller.home, ".local", "share", "claude-code-router")
	for _, directory := range []string{caller.home, caller.temporary, filepath.Dir(caller.cli)} {
		if err = os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(caller.cli, []byte("ccr fixture "+label), 0o700); err != nil {
		t.Fatal(err)
	}
	caller.values = []string{"HOME=" + caller.home, "PATH=" + filepath.Dir(caller.cli), "TMPDIR=" + caller.temporary, "DELEGATE_UNRELATED=" + label}
	return caller
}

// withExplicitSelectors adds absolute native configuration and CCR data
// selectors below the caller's own root.
func (c ccrCaller) withExplicitSelectors() ccrCaller {
	c.routerRoot = filepath.Join(c.root, "xdg-data", "claude-code-router")
	c.values = append(slices.Clone(c.values), "CLAUDE_CONFIG_DIR="+filepath.Join(c.root, "claude-config"), "XDG_DATA_HOME="+filepath.Join(c.root, "xdg-data"))
	return c
}

// predecessorFamily is one supported recorded CCR predicate with the
// predicate, launch shape, and inspection a new successor must use.
type predecessorFamily struct {
	name              string
	reference, target task.PredicateRef
	route, legacy     bool
}

func predecessorFamilies(mode string) []predecessorFamily {
	return []predecessorFamily{
		{name: "v6 route", reference: Reference(mode), target: Reference(mode), route: true},
		{name: "v6 modeled", reference: modeledReference(mode), target: modeledReference(mode)},
		{name: "v5 route", reference: scopedReference(mode, true), target: Reference(mode), route: true},
		{name: "v5 modeled", reference: scopedReference(mode, false), target: modeledReference(mode)},
		{name: "v4 route", reference: preAttributionReference(mode, true), target: Reference(mode), route: true},
		{name: "v4 modeled", reference: preAttributionReference(mode, false), target: modeledReference(mode)},
		{name: "v3 route", reference: preCredentialReference(mode, true), target: preCredentialReference(mode, true), route: true},
		{name: "v3 modeled", reference: preCredentialReference(mode, false), target: preCredentialReference(mode, false)},
		{name: "v2", reference: previousReference(mode), target: previousReference(mode), legacy: true},
		{name: "v1", reference: HistoricalReference(mode), target: HistoricalReference(mode), legacy: true},
	}
}

// facts is the launch-help projection the family's recorded inspection
// accepts; opposite is the other route projection, which a legacy inspection
// refuses rather than change the launch.
func (f predecessorFamily) facts(t *testing.T, candidate commonprovider.ProfileCandidate) json.RawMessage {
	t.Helper()
	if f.legacy {
		return legacyFacts(t, candidate)
	}
	return routeFacts(t, candidate, f.route)
}

func (f predecessorFamily) opposite(t *testing.T, candidate commonprovider.ProfileCandidate) json.RawMessage {
	t.Helper()
	return routeFacts(t, candidate, !f.route)
}

// successorModel is the model the successor requests: a route-preserving
// continuation cannot change it, while a modeled one may override it.
func (f predecessorFamily) successorModel() string {
	if f.route {
		return predecessorModel
	}
	return "successor-alias"
}

const predecessorModel = "predecessor-alias"

// familyRequests returns the predecessor's fresh request and its successor's
// exact-session continuation request for the family.
func familyRequests(t *testing.T, mode string, family predecessorFamily) (task.TaskRecord, task.TaskRecord) {
	t.Helper()
	successor := successorRequest(t, mode)
	successor.RequestedConfig.Model = family.successorModel()
	predecessor := successor
	predecessor.TaskID, predecessor.PriorSession = successor.PriorSession.PredecessorTaskID, nil
	predecessor.RequestedConfig.Model = predecessorModel
	return predecessor, successor
}

// admitFamilyPredecessor admits a fresh task under caller and records it under
// the family's predicate, as a task sealed by that revision recorded its own
// caller's launch environment and policy.
func admitFamilyPredecessor(t *testing.T, request task.TaskRecord, caller ccrCaller, family predecessorFamily) (commonprovider.PreparedProfile, task.MetaRecord) {
	t.Helper()
	candidate, err := PrepareCandidate(request, caller.values)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(routeFacts(t, candidate, family.route), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	meta := admittedCCRRecord(profile)
	meta.Predicate = family.reference
	return profile, meta
}

// admittedCCRRecord is the metadata a task admitted with profile saves.
func admittedCCRRecord(profile commonprovider.PreparedProfile) task.MetaRecord {
	return task.MetaRecord{
		EffectiveConfig: profile.Effective, Environment: slices.Clone(profile.Plan.Environment), EnvironmentRecorded: true, Predicate: profile.Plan.Predicate,
	}
}

func canonicalMeta(t *testing.T, meta task.MetaRecord) []byte {
	t.Helper()
	encoded, err := task.MarshalCanonical(meta)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// prepareFamilySuccessor prepares and finalizes the successor of meta under
// values and requires the predecessor record to stay byte-identical.
func prepareFamilySuccessor(t *testing.T, request task.TaskRecord, meta task.MetaRecord, values []string, family predecessorFamily) (commonprovider.ProfileCandidate, commonprovider.PreparedProfile) {
	t.Helper()
	before := canonicalMeta(t, meta)
	candidate, err := PrepareSuccessorCandidate(request, meta, values)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(family.facts(t, candidate), time.Now())
	if err != nil {
		t.Fatalf("recorded capability was refused: %v", err)
	}
	if !bytes.Equal(before, canonicalMeta(t, meta)) {
		t.Fatal("predecessor record changed")
	}
	return candidate, profile
}

func expectedArguments(mode, model string, route bool) []string {
	permission := "plan"
	if mode == "workspace-write" {
		permission = "bypassPermissions"
	}
	prefix := []string{"launch", "--model", model, "--auth-mode", "provider-only"}
	if route {
		prefix = []string{"launch", "--resume-route"}
	}
	return append(prefix, "--print", "--permission-mode", permission, "--", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none", "--resume", "recorded-session")
}

// requireFamilyInspection requires the recorded family's own inspection
// contract: the historical v1/v2 help probe or the route-capability probe.
func requireFamilyInspection(t *testing.T, label string, candidate commonprovider.ProfileCandidate, family predecessorFamily) {
	t.Helper()
	// The historical help probe keeps the common runtime revision it was sealed
	// under; the route probe carries its own.
	want, revision := []string{"launch", "--help"}, routeInspectionRevision
	if family.legacy {
		want, revision = []string{"launch", "--", "--help"}, commonprovider.RuntimeInspectionRevision
	}
	if candidate.Inspection == nil || !slices.Equal(candidate.Inspection.Arguments, want) || candidate.Inspection.Revision != revision {
		t.Fatalf("%s inspection changed its recorded contract: %+v", label, candidate.Inspection)
	}
}

// requireCallerLaunch asserts that candidate and its admission launch the
// caller's ccr with values and writable roots derived from the caller alone,
// and that nothing below another caller's root or outside the native filter
// reaches them.
func requireCallerLaunch(t *testing.T, label string, candidate commonprovider.ProfileCandidate, admitted commonprovider.PreparedProfile, caller ccrCaller, others ...ccrCaller) {
	t.Helper()
	if candidate.Inspection == nil || !slices.Equal(candidate.Inspection.Environment, admitted.Plan.Environment) {
		t.Fatalf("%s inspection and launch environments differ", label)
	}
	if admitted.Plan.Executable != caller.cli || admitted.Plan.ExecutableSHA256 != "" ||
		candidate.Inspection.Executable != caller.cli || candidate.Inspection.ExecutableSHA256 != admitted.Plan.ExecutableSHA256 {
		t.Fatalf("%s executable %q want the caller's %q", label, admitted.Plan.Executable, caller.cli)
	}
	for _, want := range []string{"HOME=" + caller.home, "PATH=" + filepath.Dir(caller.cli), "TMPDIR=" + caller.temporary} {
		if !slices.Contains(admitted.Plan.Environment, want) {
			t.Fatalf("%s launch environment lacks the caller's %s", label, strings.SplitN(want, "=", 2)[0])
		}
	}
	for _, want := range []string{caller.temporary, filepath.Join(caller.home, ".claude"), caller.routerRoot} {
		if !slices.Contains(admitted.WritableRoots, want) {
			t.Fatalf("%s writable roots %q lack the caller's %q", label, admitted.WritableRoots, want)
		}
	}
	if !slices.Equal(candidate.WritableRoots, admitted.WritableRoots) || admitted.Effective.Policy == nil ||
		!slices.Equal(admitted.Effective.Policy.WritableRoots, admitted.WritableRoots) || admitted.Effective.Policy.RuntimeSHA256 != admitted.Plan.ExecutableSHA256 {
		t.Fatalf("%s candidate, admission, and policy roots or runtime differ", label)
	}
	requireNoForeignLaunchEntries(t, label, admitted, others)
}

func requireNoForeignLaunchEntries(t *testing.T, label string, admitted commonprovider.PreparedProfile, others []ccrCaller) {
	t.Helper()
	for _, entry := range admitted.Plan.Environment {
		if strings.HasPrefix(entry, "DELEGATE_UNRELATED=") || strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") {
			t.Fatalf("%s launch environment kept unfiltered %q", label, strings.SplitN(entry, "=", 2)[0])
		}
	}
	for _, other := range others {
		for _, entry := range admitted.Plan.Environment {
			if strings.Contains(entry, other.root) {
				t.Fatalf("%s launch environment leaked another caller into %q", label, strings.SplitN(entry, "=", 2)[0])
			}
		}
		for _, root := range admitted.WritableRoots {
			if strings.HasPrefix(root, other.root) {
				t.Fatalf("%s writable root %q belongs to another caller", label, root)
			}
		}
	}
}

// requireRecordReconstruction asserts that reconstructing an admitted record
// under another caller keeps the record's own predicate, launch environment,
// and roots while the reconstructing caller resolves the executable. It
// returns the reconstructed profile.
func requireRecordReconstruction(t *testing.T, label string, request task.TaskRecord, meta task.MetaRecord, family predecessorFamily, caller ccrCaller) commonprovider.PreparedProfile {
	t.Helper()
	before := canonicalMeta(t, meta)
	candidate, err := PrepareExistingCandidate(request, meta, caller.values)
	if err != nil {
		t.Fatalf("%s reconstruction: %v", label, err)
	}
	requireFamilyInspection(t, label+" reconstruction", candidate, family)
	rebuilt, err := candidate.Finalize(family.facts(t, candidate), time.Now())
	if err != nil {
		t.Fatalf("%s reconstruction refused its recorded capability: %v", label, err)
	}
	if !slices.Equal(rebuilt.Plan.Environment, meta.Environment) || !slices.Equal(rebuilt.WritableRoots, meta.EffectiveConfig.Policy.WritableRoots) ||
		!rebuilt.Plan.Predicate.Equal(meta.Predicate) {
		t.Fatalf("%s reconstruction did not keep its own saved predicate, launch, and roots", label)
	}
	if rebuilt.Plan.Executable != caller.cli {
		t.Fatalf("%s reconstruction executable %q want the reconstructing caller's %q", label, rebuilt.Plan.Executable, caller.cli)
	}
	if !bytes.Equal(before, canonicalMeta(t, meta)) {
		t.Fatalf("%s record changed", label)
	}
	return rebuilt
}

// TestSuccessorLaunchesFromCallerWhileAdmittedRecordsKeepTheirOwn is the
// central continuation regression for every supported predecessor family: A
// is admitted under caller A, caller B creates successor B, and caller C later
// reconstructs A from A's record and B from B's record.
func TestSuccessorLaunchesFromCallerWhileAdmittedRecordsKeepTheirOwn(t *testing.T) {
	for _, mode := range successorModes {
		for _, family := range predecessorFamilies(mode) {
			t.Run(mode+"/"+family.name, func(t *testing.T) {
				testSuccessorLaunchesFromCaller(t, mode, family)
			})
		}
	}
}

func testSuccessorLaunchesFromCaller(t *testing.T, mode string, family predecessorFamily) {
	t.Helper()
	a, b, c := newCCRCaller(t, "a"), newCCRCaller(t, "b"), newCCRCaller(t, "c")
	predecessorRequest, request := familyRequests(t, mode, family)
	predecessor, predecessorMeta := admitFamilyPredecessor(t, predecessorRequest, a, family)
	if err := validateContinuation(predecessorRequest, predecessorMeta, request); err != nil {
		t.Fatalf("continuation validator refused the family's request: %v", err)
	}

	candidate, successor := prepareFamilySuccessor(t, request, predecessorMeta, b.values, family)
	requireFamilyInspection(t, "successor", candidate, family)
	requireCallerLaunch(t, "successor", candidate, successor, b, a, c)
	if !successor.Plan.Predicate.Equal(family.target) {
		t.Fatalf("successor predicate %+v want %+v", successor.Plan.Predicate, family.target)
	}
	if want := expectedArguments(mode, family.successorModel(), family.route); !slices.Equal(successor.Plan.Arguments, want) {
		t.Fatalf("successor argv %q want %q", successor.Plan.Arguments, want)
	}
	if family.legacy {
		if _, err := candidate.Finalize(family.opposite(t, candidate), time.Now()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
			t.Fatalf("legacy inspection accepted a route projection: %v", err)
		}
	} else {
		requireOppositeRouteCapability(t, "successor", candidate, successor, family.route)
	}

	// A reconstructs from its own request: its explicit model and fresh
	// session, not B's exact-session continuation.
	reconstructed := requireRecordReconstruction(t, "predecessor", predecessorRequest, predecessorMeta, family, c)
	if !slices.Equal(reconstructed.Plan.Arguments, predecessor.Plan.Arguments) {
		t.Fatalf("predecessor reconstruction argv %q want its admitted %q", reconstructed.Plan.Arguments, predecessor.Plan.Arguments)
	}
	successorFamily := family
	successorFamily.reference = family.target
	requireRecordReconstruction(t, "successor", request, admittedCCRRecord(successor), successorFamily, c)
}

// TestSuccessorOfCurrentTargetMatchesFreshCallerAdmission compares each
// successor that continues under a current predicate with the fresh admission
// of the same request in the same caller, whose route probe selects that
// predicate under the same capability proof.
func TestSuccessorOfCurrentTargetMatchesFreshCallerAdmission(t *testing.T) {
	for _, mode := range successorModes {
		for _, family := range predecessorFamilies(mode) {
			if !family.target.Equal(Reference(mode)) && !family.target.Equal(modeledReference(mode)) {
				continue
			}
			t.Run(mode+"/"+family.name, func(t *testing.T) {
				a, b := newCCRCaller(t, "a"), newCCRCaller(t, "b")
				predecessorRequest, request := familyRequests(t, mode, family)
				_, predecessorMeta := admitFamilyPredecessor(t, predecessorRequest, a, family)
				candidate, successor := prepareFamilySuccessor(t, request, predecessorMeta, b.values, family)
				fresh, err := PrepareCandidate(request, b.values)
				if err != nil {
					t.Fatal(err)
				}
				freshProfile, err := fresh.Finalize(family.facts(t, fresh), time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if inspectionDigest(t, candidate) != inspectionDigest(t, fresh) || !reflect.DeepEqual(successor.Plan, freshProfile.Plan) ||
					!task.CompareEffectiveConfigs(successor.Effective, freshProfile.Effective) || !slices.Equal(successor.WritableRoots, freshProfile.WritableRoots) {
					t.Fatal("successor differs from the fresh admission of the same caller")
				}
			})
		}
	}
}

// TestSuccessorRefusesInvalidCallerEnvironmentWithoutPredecessorFallback
// requires current native validation of the caller's environment: a refused
// caller never launches from, or locates ccr through, the predecessor's valid
// recorded environment, which still reconstructs.
func TestSuccessorRefusesInvalidCallerEnvironmentWithoutPredecessorFallback(t *testing.T) {
	for _, mode := range successorModes {
		for _, family := range predecessorFamilies(mode) {
			t.Run(mode+"/"+family.name, func(t *testing.T) {
				testSuccessorRefusesInvalidCaller(t, mode, family)
			})
		}
	}
}

func testSuccessorRefusesInvalidCaller(t *testing.T, mode string, family predecessorFamily) {
	t.Helper()
	a, b, c := newCCRCaller(t, "a"), newCCRCaller(t, "b"), newCCRCaller(t, "c")
	predecessorRequest, request := familyRequests(t, mode, family)
	_, predecessorMeta := admitFamilyPredecessor(t, predecessorRequest, a, family)
	without := func(key string) []string {
		return slices.DeleteFunc(slices.Clone(b.values), func(entry string) bool { return strings.HasPrefix(entry, key+"=") })
	}
	invalid := map[string][]string{
		"empty":                    {},
		"missing HOME":             without("HOME"),
		"relative HOME":            append(without("HOME"), "HOME=relative/home"),
		"malformed entry":          append(slices.Clone(b.values), "MALFORMED"),
		"duplicate key":            append(slices.Clone(b.values), "HOME="+a.home),
		"relative native selector": append(slices.Clone(b.values), "CLAUDE_CONFIG_DIR=relative/claude"),
		"relative CCR data root":   append(slices.Clone(b.values), "XDG_DATA_HOME=relative/data"),
	}
	before := canonicalMeta(t, predecessorMeta)
	for name, values := range invalid {
		if _, err := PrepareSuccessorCandidate(request, predecessorMeta, values); !errors.Is(err, claude.ErrUnsupportedProfile) {
			t.Fatalf("%s: successor error=%v want %v without falling back to the predecessor environment", name, err, claude.ErrUnsupportedProfile)
		}
	}
	// The predecessor's recorded PATH holds a ccr; a caller without PATH must
	// not locate it there.
	if _, err := PrepareSuccessorCandidate(request, predecessorMeta, without("PATH")); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing PATH: successor error=%v want %v", err, exec.ErrNotFound)
	}
	if !bytes.Equal(before, canonicalMeta(t, predecessorMeta)) {
		t.Fatal("predecessor record changed")
	}
	requireRecordReconstruction(t, "predecessor", predecessorRequest, predecessorMeta, family, c)
}

// TestSuccessorEnvironmentRecordEligibility keeps the recorded-environment
// guard as predecessor eligibility: a record without one is refused, while a
// record that saved an empty environment continues from a valid caller. The
// reconstruction of the same records keeps its existing policy.
func TestSuccessorEnvironmentRecordEligibility(t *testing.T) {
	for _, mode := range successorModes {
		for _, family := range predecessorFamilies(mode) {
			t.Run(mode+"/"+family.name, func(t *testing.T) {
				testSuccessorEnvironmentRecordEligibility(t, mode, family)
			})
		}
	}
}

func testSuccessorEnvironmentRecordEligibility(t *testing.T, mode string, family predecessorFamily) {
	t.Helper()
	a, b, c := newCCRCaller(t, "a"), newCCRCaller(t, "b"), newCCRCaller(t, "c")
	predecessorRequest, request := familyRequests(t, mode, family)
	_, admitted := admitFamilyPredecessor(t, predecessorRequest, a, family)
	unrecorded, decodedEmpty, savedEmpty := admitted, admitted, admitted
	unrecorded.Environment, unrecorded.EnvironmentRecorded = nil, false
	decodedEmpty.Environment = nil
	savedEmpty.Environment = []string{}

	for name, prepare := range map[string]commonprovider.PrepareExistingCandidate{"existing": PrepareExistingCandidate, "successor": PrepareSuccessorCandidate} {
		if _, err := prepare(request, unrecorded, b.values); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("%s without a recorded environment error=%v want %v", name, err, task.ErrIdentityMismatch)
		}
	}
	for name, meta := range map[string]task.MetaRecord{"decoded empty": decodedEmpty, "saved empty": savedEmpty} {
		candidate, successor := prepareFamilySuccessor(t, request, meta, b.values, family)
		requireFamilyInspection(t, name+" successor", candidate, family)
		requireCallerLaunch(t, name+" successor", candidate, successor, b, a, c)
		if !successor.Plan.Predicate.Equal(family.target) {
			t.Fatalf("%s successor predicate %+v want %+v", name, successor.Plan.Predicate, family.target)
		}
		if _, err := PrepareExistingCandidate(predecessorRequest, meta, c.values); !errors.Is(err, claude.ErrUnsupportedProfile) {
			t.Fatalf("%s reconstruction error=%v want %v", name, err, claude.ErrUnsupportedProfile)
		}
	}
}

// TestSuccessorInspectionDefinitionRoundTripsThroughItsFilteredEnvironment
// prepares each family's successor from a raw caller environment, then again
// from the filtered environment its inspection records, for the default and
// explicit native configuration and CCR data selectors.
func TestSuccessorInspectionDefinitionRoundTripsThroughItsFilteredEnvironment(t *testing.T) {
	for _, mode := range successorModes {
		for _, family := range predecessorFamilies(mode) {
			t.Run(mode+"/"+family.name+"/default", func(t *testing.T) {
				testSuccessorFilteredRoundTrip(t, mode, family, newCCRCaller(t, "a"), newCCRCaller(t, "b"), nil)
			})
			t.Run(mode+"/"+family.name+"/explicit", func(t *testing.T) {
				b := newCCRCaller(t, "b").withExplicitSelectors()
				selected := []string{filepath.Join(b.root, "claude-config"), filepath.Join(b.root, "xdg-data", "claude"), b.routerRoot}
				testSuccessorFilteredRoundTrip(t, mode, family, newCCRCaller(t, "a").withExplicitSelectors(), b, selected)
			})
		}
	}
}

func testSuccessorFilteredRoundTrip(t *testing.T, mode string, family predecessorFamily, a, b ccrCaller, selectedRoots []string) {
	t.Helper()
	raw := append(slices.Clone(b.values), "ANTHROPIC_API_KEY=fixture-secret")
	predecessorRequest, request := familyRequests(t, mode, family)
	_, predecessorMeta := admitFamilyPredecessor(t, predecessorRequest, a, family)

	first, firstAdmission := prepareFamilySuccessor(t, request, predecessorMeta, raw, family)
	requireCallerLaunch(t, "raw successor", first, firstAdmission, b, a)
	for _, want := range selectedRoots {
		if !slices.Contains(firstAdmission.WritableRoots, want) {
			t.Fatalf("explicit selector root %q missing from %q", want, firstAdmission.WritableRoots)
		}
	}
	second, secondAdmission := prepareFamilySuccessor(t, request, predecessorMeta, slices.Clone(first.Inspection.Environment), family)
	if inspectionDigest(t, first) != inspectionDigest(t, second) || !slices.Equal(first.WritableRoots, second.WritableRoots) {
		t.Fatal("filtered successor environment changed the inspection definition")
	}
	if !reflect.DeepEqual(firstAdmission.Plan, secondAdmission.Plan) || !task.CompareEffectiveConfigs(firstAdmission.Effective, secondAdmission.Effective) ||
		!slices.Equal(firstAdmission.WritableRoots, secondAdmission.WritableRoots) {
		t.Fatal("filtered successor environment changed the admission")
	}

	reconstructed, err := PrepareExistingCandidate(predecessorRequest, predecessorMeta, raw)
	if err != nil {
		t.Fatal(err)
	}
	if inspectionDigest(t, reconstructed) == inspectionDigest(t, first) {
		t.Fatal("successor reused the predecessor's inspection definition")
	}
}

// TestSuccessorContinuationValidatorPairsWithRouteCapability pairs the
// route-preserving model restriction with the route capability the successor
// requires under its caller: the model cannot change, and a caller whose ccr
// lacks --resume-route is refused instead of falling back to a modeled launch.
// A modeled predecessor accepts the override and launches with it.
func TestSuccessorContinuationValidatorPairsWithRouteCapability(t *testing.T) {
	for _, mode := range successorModes {
		for _, family := range predecessorFamilies(mode) {
			t.Run(mode+"/"+family.name, func(t *testing.T) {
				testSuccessorValidatorPair(t, mode, family)
			})
		}
	}
}

func testSuccessorValidatorPair(t *testing.T, mode string, family predecessorFamily) {
	t.Helper()
	a, b := newCCRCaller(t, "a"), newCCRCaller(t, "b")
	predecessorRequest, request := familyRequests(t, mode, family)
	_, predecessorMeta := admitFamilyPredecessor(t, predecessorRequest, a, family)
	changed := request
	changed.RequestedConfig.Model = "changed-alias"
	err := validateContinuation(predecessorRequest, predecessorMeta, changed)
	if family.route != errors.Is(err, commonprovider.ErrUnsupportedOption) || (!family.route && err != nil) {
		t.Fatalf("model change error=%v for route=%t", err, family.route)
	}
	if !family.route {
		_, successor := prepareFamilySuccessor(t, changed, predecessorMeta, b.values, family)
		if want := expectedArguments(mode, "changed-alias", false); !slices.Equal(successor.Plan.Arguments, want) {
			t.Fatalf("modeled successor argv %q want %q", successor.Plan.Arguments, want)
		}
		return
	}
	candidate, err := PrepareSuccessorCandidate(request, predecessorMeta, b.values)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = candidate.Finalize(routeFacts(t, candidate, false), time.Now()); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("route-preserving successor without --resume-route error=%v want %v", err, commonprovider.ErrProfileUnavailable)
	}
}
