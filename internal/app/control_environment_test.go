package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// appTestControlEnvironment prepares the fixture-owned control snapshot before
// any consumer starts. Finite race-instrumented owners keep diagnostics and
// explicit exit delays, without paying the runtime's default exit delay.
func appTestControlEnvironment(environment []string) []string {
	raceOptions := verifiedexec.EnvironmentValue(environment, "GORACE")
	if !strings.Contains(raceOptions, "atexit_sleep_ms=") {
		if raceOptions != "" {
			raceOptions += " "
		}
		raceOptions += "atexit_sleep_ms=0"
	}
	return append(pueue.DefaultEnvironmentFrom(environment), "GORACE="+raceOptions)
}

func TestAppControlFixturePreservesRaceDiagnostics(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"default", "", "atexit_sleep_ms=0"},
		{"diagnostics", "halt_on_error=1 exitcode=73", "halt_on_error=1 exitcode=73 atexit_sleep_ms=0"},
		{"explicit delay", "atexit_sleep_ms=1500", "atexit_sleep_ms=1500"},
		{"explicit zero", "exitcode=73:atexit_sleep_ms=0", "exitcode=73:atexit_sleep_ms=0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			environment := []string{"HOME=/fixture"}
			if tc.input != "" {
				environment = append(environment, "GORACE="+tc.input)
			}
			before := slices.Clone(environment)
			var got []string
			for _, entry := range appTestControlEnvironment(environment) {
				if value, found := strings.CutPrefix(entry, "GORACE="); found {
					got = append(got, value)
				}
			}
			if !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("fixture race options = %q, want exactly %q", got, tc.want)
			}
			if !slices.Equal(environment, before) {
				t.Fatal("control preparation changed its captured environment")
			}
		})
	}
}
