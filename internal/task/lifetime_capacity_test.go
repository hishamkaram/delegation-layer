package task

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func capacityTestLaunch(sequence uint64, processStart string) LifetimeLaunchRecord {
	return LifetimeLaunchRecord{
		SchemaVersion: SchemaVersion, RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32),
		RegistrationSHA256: strings.Repeat("c", 64), Sequence: sequence, PID: 100 + int(sequence),
		ProcessStart: processStart, PGID: 100 + int(sequence), StartedAt: "2026-01-02T03:04:05Z",
	}
}

func canonicalLaunchLength(t *testing.T, record LifetimeLaunchRecord) int {
	t.Helper()
	data, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	return len(data)
}

func TestLifetimeLaunchCapacityConstants(t *testing.T) {
	if MaxLifetimeLaunches != 4096 {
		t.Fatalf("MaxLifetimeLaunches = %d, want 4096", MaxLifetimeLaunches)
	}
	if MaxLifetimeLaunchBytes != 64*MaxControlRecordSize {
		t.Fatalf("MaxLifetimeLaunchBytes = %d, want 64 * MaxControlRecordSize", MaxLifetimeLaunchBytes)
	}
}

func TestLifetimeLaunchCapacityCountBoundary(t *testing.T) {
	launches := make([]LifetimeLaunchRecord, 0, MaxLifetimeLaunches+1)
	for sequence := uint64(1); sequence <= MaxLifetimeLaunches; sequence++ {
		launches = append(launches, capacityTestLaunch(sequence, "start"))
	}
	if err := ValidateLifetimeLaunchCapacity(launches); err != nil {
		t.Fatalf("full namespace refused: %v", err)
	}
	var capacity LifetimeLaunchCapacity
	for index := range launches {
		if err := capacity.Add(&launches[index]); err != nil {
			t.Fatalf("launch %d refused: %v", index+1, err)
		}
	}
	extra := capacityTestLaunch(MaxLifetimeLaunches+1, "start")
	if err := capacity.Add(&extra); !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("launch %d = %v, want evidence fault", MaxLifetimeLaunches+1, err)
	}
	if err := ValidateLifetimeLaunchCapacity(append(launches, extra)); !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("assembled set of %d = %v, want evidence fault", MaxLifetimeLaunches+1, err)
	}
	for _, sequence := range []uint64{1, MaxLifetimeLaunches} {
		if err := ValidateLifetimeLaunchSequence(sequence); err != nil {
			t.Fatalf("sequence %d refused: %v", sequence, err)
		}
	}
	for _, sequence := range []uint64{0, MaxLifetimeLaunches + 1} {
		if err := ValidateLifetimeLaunchSequence(sequence); !errors.Is(err, ErrEvidenceFault) {
			t.Fatalf("sequence %d = %v, want evidence fault", sequence, err)
		}
	}
}

// The byte limit counts canonical encoding, including HTML escaping that
// expands '<', '>' and '&' sixfold, and UTF-8 bytes rather than characters.
func TestLifetimeLaunchCapacityCountsCanonicalBytes(t *testing.T) {
	escaped := capacityTestLaunch(1, "<>&é✓")
	plain := capacityTestLaunch(2, "plain")
	escapedLength := canonicalLaunchLength(t, escaped)
	if want := canonicalLaunchLength(t, capacityTestLaunch(1, "")) + 3*6 + len("é✓"); escapedLength != want {
		t.Fatalf("escaped canonical length = %d, want %d", escapedLength, want)
	}
	exact := escapedLength + canonicalLaunchLength(t, plain)
	capacity := LifetimeLaunchCapacity{limits: &lifetimeLaunchLimits{count: 10, bytes: exact}}
	if err := capacity.Add(&escaped); err != nil {
		t.Fatal(err)
	}
	if err := capacity.Add(&plain); err != nil {
		t.Fatalf("exact byte budget refused: %v", err)
	}
	if err := capacity.Add(&plain); !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("over byte budget = %v, want evidence fault", err)
	}

	oneOver := LifetimeLaunchCapacity{limits: &lifetimeLaunchLimits{count: 10, bytes: exact - 1}}
	if err := oneOver.Add(&escaped); err != nil {
		t.Fatal(err)
	}
	if err := oneOver.Add(&plain); !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("one byte over = %v, want evidence fault", err)
	}
}

// A refused record leaves the accumulator unchanged for later records.
func TestLifetimeLaunchCapacityRefusalLeavesAccumulatorUnchanged(t *testing.T) {
	capacity := LifetimeLaunchCapacity{limits: &lifetimeLaunchLimits{count: 2, bytes: 10}}
	if err := capacity.addEncoded(6); err != nil {
		t.Fatal(err)
	}
	if err := capacity.addEncoded(5); !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("over budget = %v, want evidence fault", err)
	}
	if err := capacity.addEncoded(4); err != nil {
		t.Fatalf("refusal changed the byte total: %v", err)
	}
	if err := capacity.addEncoded(0); !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("over count = %v, want evidence fault", err)
	}
}

func TestLifetimeLaunchCapacityRefusesOversizedRecord(t *testing.T) {
	record := capacityTestLaunch(1, strings.Repeat("x", MaxControlRecordSize))
	if err := ValidateLifetimeLaunchCapacity([]LifetimeLaunchRecord{record}); !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("oversized record = %v, want evidence fault", err)
	}
}

// A nonempty canonical set is its records' canonical bytes plus 2: the
// brackets and N-1 commas replace N-1 of the N record newlines. The empty set
// is exactly "[]\n".
func TestLifetimeLaunchSetCanonicalLengthMatchesRecordSum(t *testing.T) {
	launches := []LifetimeLaunchRecord{
		capacityTestLaunch(1, strings.Repeat("<", 512)),
		capacityTestLaunch(2, "é✓&"),
		capacityTestLaunch(3, "plain"),
	}
	sum := 0
	for _, launch := range launches {
		sum += canonicalLaunchLength(t, launch)
	}
	set, err := MarshalCanonical(launches)
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != sum+2 {
		t.Fatalf("canonical set length = %d, want record sum %d + 2", len(set), sum)
	}
	empty, err := MarshalCanonical([]LifetimeLaunchRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != "[]\n" {
		t.Fatalf("empty canonical set = %q, want %q", empty, "[]\n")
	}
}

// The launch-set digest protocol is unchanged: compact json.Marshal of the
// ordered array with HTML escaping and one trailing newline, nil as [].
func TestLifetimeLaunchSetDigestPreservesHistoricalBytes(t *testing.T) {
	launches := []LifetimeLaunchRecord{capacityTestLaunch(1, "<start>&é"), capacityTestLaunch(2, "next")}
	data, err := json.Marshal(launches)
	if err != nil {
		t.Fatal(err)
	}
	historical := ComputeSHA256(append(data, '\n'))
	got, err := LifetimeLaunchSetDigest(launches)
	if err != nil {
		t.Fatal(err)
	}
	if got != historical {
		t.Fatalf("digest = %s, want historical %s", got, historical)
	}
	if !strings.Contains(string(data), "\\u003cstart\\u003e\\u0026") {
		t.Fatalf("canonical set lost HTML escaping: %s", data)
	}
	emptyDigest := ComputeSHA256([]byte("[]\n"))
	for name, set := range map[string][]LifetimeLaunchRecord{"nil": nil, "empty": {}} {
		digest, digestErr := LifetimeLaunchSetDigest(set)
		if digestErr != nil || digest != emptyDigest {
			t.Fatalf("%s set digest = %s, %v; want digest of []", name, digest, digestErr)
		}
	}
}

func TestLifetimeEvidenceRefusesLaunchesBeyondCapacity(t *testing.T) {
	launches := make([]LifetimeLaunchRecord, 0, MaxLifetimeLaunches+1)
	for sequence := uint64(1); sequence <= MaxLifetimeLaunches+1; sequence++ {
		launches = append(launches, capacityTestLaunch(sequence, "start"))
	}
	if err := validateLifetimeEvidenceRecords(LifetimeEvidence{Launches: launches}); !errors.Is(err, ErrEvidenceFault) {
		t.Fatalf("evidence beyond capacity = %v, want evidence fault", err)
	}
}
