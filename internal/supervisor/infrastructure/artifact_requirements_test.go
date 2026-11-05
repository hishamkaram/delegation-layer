package infrastructure

import (
	"errors"
	"strings"
	"testing"
)

func TestArtifactRequirementsValidate(t *testing.T) {
	valid := ArtifactRequirements{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		for index := range 3 {
			changed := valid
			fields := []*string{&changed.RequiredRuntimeSHA256, &changed.RequiredClientSHA256, &changed.RequiredDaemonSHA256}
			*fields[index] = invalid
			if err := changed.Validate(); !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid required digest %d accepted: %v", index, err)
			}
		}
	}
}

func TestArtifactRequirementsCompareVerifiedHashes(t *testing.T) {
	required := ArtifactRequirements{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	actual := Artifacts{RunnerSHA256: strings.Repeat("a", 64), ClientSHA256: strings.Repeat("b", 64), DaemonSHA256: strings.Repeat("c", 64)}
	before := required
	if err := required.ValidateArtifacts(actual); err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		changed := actual
		fields := []*string{&changed.RunnerSHA256, &changed.ClientSHA256, &changed.DaemonSHA256}
		*fields[index] = strings.Repeat("d", 64)
		if err := required.ValidateArtifacts(changed); !errors.Is(err, ErrIncompatible) || errors.Is(err, ErrIdentity) {
			t.Fatalf("valid incompatible source %d misclassified: %v", index, err)
		}
		*fields[index] = ""
		if err := required.ValidateArtifacts(changed); !errors.Is(err, ErrIdentity) {
			t.Fatalf("malformed source digest %d accepted: %v", index, err)
		}
	}
	if err := (ArtifactRequirements{}).ValidateArtifacts(actual); !errors.Is(err, ErrIdentity) {
		t.Fatalf("missing requirements accepted: %v", err)
	}
	if required != before {
		t.Fatal("comparison mutated request constraints")
	}
}
