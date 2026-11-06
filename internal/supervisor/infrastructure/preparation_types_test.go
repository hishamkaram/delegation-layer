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
