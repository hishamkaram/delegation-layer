package app

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// modelsFailureCanary is embedded in every injected cause. No public models
// response may contain it.
const modelsFailureCanary = "sk-models-failure-canary-7f3a"

// modelsFailureCatalog builds a discoverable fixture provider whose Prepare
// step is supplied by the test. workspace is the canonical provider directory.
func modelsFailureCatalog(t *testing.T, workspace string, prepareErr error) commonprovider.Catalog {
	t.Helper()
	interpreter, err := predicate.Default().Resolve(task.FixturePredicateRef())
	if err != nil {
		t.Fatal(err)
	}
	inspection := commonprovider.InspectionDefinition{
		Revision: commonprovider.RuntimeInspectionRevision, Executable: "/fake/provider", ExecutableSHA256: task.ComputeSHA256([]byte("fake")),
		Directory: workspace, Environment: []string{}, OutputLimit: commonprovider.MaxInspectionOutput,
		Runtime: &commonprovider.RuntimeProbeDefinition{Executable: "/fake/provider", ExecutableSHA256: task.ComputeSHA256([]byte("fake")), Directory: workspace, Environment: []string{}, RequiredFlags: []string{"--fixture"}},
	}
	catalog, err := commonprovider.NewCatalog(commonprovider.Registration{
		Description: commonprovider.Description{
			ID:             config.ProviderFixture,
			SupportedModes: []string{config.ModeReadOnly},
			ReadOnly:       commonprovider.ReadOnlyCapability{Supported: true, Mechanism: commonprovider.ReadOnlyNativePlan, Containment: commonprovider.ReadOnlyProviderOwned},
			Discoverable:   true,
			Runtime:        commonprovider.RuntimeCapability{RequiredFlags: []string{"--fixture"}},
		},
		Interpreters: []predicate.Interpreter{interpreter},
		Prepare: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			if prepareErr != nil {
				return commonprovider.ProfileCandidate{}, prepareErr
			}
			return commonprovider.ProfileCandidate{Directory: workspace, Inspection: &inspection}, nil
		},
		Models: func() commonprovider.ModelDiscoveryDefinition {
			return commonprovider.ModelDiscoveryDefinition{Arguments: []string{"models"}, Source: "fixture", Project: func([]byte, []byte, []byte, bool) (commonprovider.ModelCatalog, error) {
				t.Error("failure fixture reached provider projection")
				return commonprovider.ModelCatalog{}, nil
			}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// runModelsPublic runs the public models command in both output forms and
// returns the decoded JSON response with the raw human output.
func runModelsPublic(t *testing.T, args []string, deps Dependencies) (jsonResponse ModelsResponse, jsonCode int, human string, humanCode int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	jsonCode = Run(append(append([]string{}, args...), "--json"), &stdout, &stderr, deps)
	if stderr.Len() != 0 {
		t.Fatalf("json models wrote stderr: %q", stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &jsonResponse); err != nil {
		t.Fatalf("non-JSON response: %s (%v)", stdout.String(), err)
	}
	if bytes.Contains(stdout.Bytes(), []byte(modelsFailureCanary)) {
		t.Fatalf("json response leaked the canary: %s", stdout.String())
	}
	stdout.Reset()
	humanCode = Run(args, &stdout, &stderr, deps)
	if stderr.Len() != 0 {
		t.Fatalf("human models wrote stderr: %q", stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte(modelsFailureCanary)) {
		t.Fatalf("human response leaked the canary: %s", stdout.String())
	}
	return jsonResponse, jsonCode, stdout.String(), humanCode
}
