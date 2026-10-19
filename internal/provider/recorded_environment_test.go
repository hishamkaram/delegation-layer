package provider

import (
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRecordedEnvironmentDistinguishesLegacyFromRecordedEmpty(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		meta         task.MetaRecord
		wantValues   []string
		wantRecorded bool
	}{
		{name: "legacy nil without flag"},
		{name: "recorded values", meta: task.MetaRecord{Environment: []string{"HOME=/saved"}, EnvironmentRecorded: true}, wantValues: []string{"HOME=/saved"}, wantRecorded: true},
		{name: "values without flag", meta: task.MetaRecord{Environment: []string{"HOME=/saved"}}, wantValues: []string{"HOME=/saved"}, wantRecorded: true},
		{name: "flag with nil slice after omitempty", meta: task.MetaRecord{EnvironmentRecorded: true}, wantRecorded: true},
		{name: "non-nil empty without flag", meta: task.MetaRecord{Environment: []string{}}, wantRecorded: true},
		{name: "non-nil empty with flag", meta: task.MetaRecord{Environment: []string{}, EnvironmentRecorded: true}, wantRecorded: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			values, recorded := RecordedEnvironment(testCase.meta)
			if recorded != testCase.wantRecorded || !slices.Equal(values, testCase.wantValues) {
				t.Fatalf("RecordedEnvironment=%q recorded=%t want=%q recorded=%t", values, recorded, testCase.wantValues, testCase.wantRecorded)
			}
			if len(values) == 0 && recorded && testCase.meta.Environment != nil && values == nil {
				t.Fatal("explicit empty environment lost its non-nil identity")
			}
		})
	}
}

func TestRecordedEnvironmentReturnsIndependentCopy(t *testing.T) {
	meta := task.MetaRecord{Environment: []string{"HOME=/saved", "PATH=/bin"}, EnvironmentRecorded: true}
	values, recorded := RecordedEnvironment(meta)
	if !recorded {
		t.Fatal("recorded environment reported as legacy")
	}
	values[0] = "HOME=/mutated"
	values = append(values[:1], "EXTRA=1")
	if !slices.Equal(meta.Environment, []string{"HOME=/saved", "PATH=/bin"}) {
		t.Fatalf("caller mutation reached the saved record: %q", meta.Environment)
	}
	if !slices.Equal(values, []string{"HOME=/mutated", "EXTRA=1"}) {
		t.Fatalf("returned copy=%q", values)
	}
}
