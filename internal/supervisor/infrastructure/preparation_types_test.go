package infrastructure

import (
	"errors"
	"strings"
	"testing"
)

func TestPreparationDispositionKeepsOneContextMeaning(t *testing.T) {
	context := LaunchContextReference{SHA256: strings.Repeat("a", 64)}
	for _, fixture := range []struct {
		name  string
		value Disposition
		valid bool
	}{
		{"started with resolved context", Disposition{Kind: DispositionStarted, Context: context}, true},
		{"warm reuse", Disposition{Kind: DispositionWarmReuse}, true},
		{"missing context", Disposition{Kind: DispositionStarted}, false},
		{"invalid context", Disposition{Kind: DispositionStarted, Context: LaunchContextReference{SHA256: "invalid"}}, false},
		{"warm with selected context", Disposition{Kind: DispositionWarmReuse, Context: context}, false},
		{"zero outcome", Disposition{}, false},
		{"unknown outcome", Disposition{Kind: "ready", Context: context}, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			err := fixture.value.Validate()
			if fixture.valid && err != nil {
				t.Fatal(err)
			}
			if !fixture.valid && !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid outcome accepted: %v", err)
			}
		})
	}
}

func TestMigrationReferenceSelectsAnExistingHistoryPosition(t *testing.T) {
	reference := MigrationReference{RootID: strings.Repeat("a", 32), RecordSHA256: strings.Repeat("b", 64)}
	if err := reference.Validate(); err != nil {
		t.Fatalf("first history offset is valid: %v", err)
	}
	later := reference
	later.HistoryIndex++
	if err := later.Validate(); err != nil || later == reference {
		t.Fatalf("different history locators must remain distinct values: %v", err)
	}
	for _, invalid := range []MigrationReference{
		{},
		{RootID: reference.RootID},
		{RootID: "../root", RecordSHA256: reference.RecordSHA256},
		{RootID: reference.RootID, RecordSHA256: "invalid"},
	} {
		if err := invalid.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid migration selector accepted: %v", err)
		}
	}
}
