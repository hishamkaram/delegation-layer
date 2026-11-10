// Package tuiprovider composes native TUI acceptance with the existing finite
// contributor provider. It is never imported by production command binaries.
package tuiprovider

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorcli"
	"github.com/hishamkaram/delegation-layer/internal/testutil/resourcecheck"
	"github.com/hishamkaram/delegation-layer/internal/testutil/tuiprovider/gate"
	"github.com/hishamkaram/delegation-layer/internal/tui"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// Run uses the production parser, typed application operations and terminal
// composition. The only extra preparation behavior is an explicitly planted,
// finite admission barrier owned by this acceptance fixture.
func Run(args []string, input *os.File, stdout, stderr io.Writer) int {
	deps, err := newDependencies()
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	deps.Input = input
	if parsed, parseErr := app.ParseArguments(args); parseErr == nil && parsed.Command == "tui" {
		return tui.Launch(parsed, deps, input, stdout, stderr)
	}
	return app.Run(args, stdout, stderr, deps)
}

// RunRunner shares the same explicit fixture launch scope as its caller. The
// ordinary runner and inspection worker still execute the application's owners.
func RunRunner(args []string, stdout, stderr io.Writer) int {
	deps, err := newDependencies()
	if err != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return 1
		}
		return 1
	}
	return app.RunRunner(args, stdout, stderr, deps)
}

func newDependencies() (app.Dependencies, error) {
	deps, err := contributorcli.NewDependencies()
	if err != nil {
		return app.Dependencies{}, err
	}
	// Established fixture isolation: native UI/lifecycle is measured here;
	// host-pressure policy is covered by the resource-guard tests.
	deps.ResourceChecker = resourcecheck.Clear()
	deps.PublisherVersion = "tui-contributor-fixture"
	prepare := deps.Catalog.Candidate
	deps.PrepareCandidate = func(request task.TaskRecord, environment []string) (provider.ProfileCandidate, error) {
		runtime := verifiedexec.EnvironmentValue(environment, "DELEGATE_CONTRIBUTOR_RUNTIME")
		if err := gate.Wait(runtime, gate.Admission, request.TaskID); err != nil {
			return provider.ProfileCandidate{}, err
		}
		candidate, err := prepare(request, environment)
		if err != nil || runtime == "" {
			return candidate, err
		}
		return bindFixtureRuntime(candidate, runtime)
	}
	return deps, nil
}

// The contributor adapter validates this nonsecret directory. Preserve that
// explicit selection across the canonical saved scope, rather than relying on
// its installation-adjacent fallback (which is shared across parallel runs).
func bindFixtureRuntime(candidate provider.ProfileCandidate, runtime string) (provider.ProfileCandidate, error) {
	if candidate.Inspection == nil || candidate.Inspection.Runtime == nil || candidate.Finalize == nil {
		return provider.ProfileCandidate{}, provider.ErrProfileUnavailable
	}
	value := "DELEGATE_CONTRIBUTOR_RUNTIME=" + runtime
	definition := *candidate.Inspection
	definition.Environment = []string{value}
	probe := *definition.Runtime
	probe.Environment = []string{value}
	definition.Runtime = &probe
	candidate.Inspection = &definition
	finalize := candidate.Finalize
	candidate.Finalize = func(facts json.RawMessage, observed time.Time) (provider.PreparedProfile, error) {
		profile, err := finalize(facts, observed)
		if err == nil {
			profile.Plan.Environment = []string{value}
		}
		return profile, err
	}
	return candidate, nil
}
