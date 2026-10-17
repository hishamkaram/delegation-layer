package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider/contract"
)

func TestProviderNamesAreTheContractTypes(t *testing.T) {
	pairs := []struct {
		name     string
		provider reflect.Type
		contract reflect.Type
	}{
		{"ContinuationMode", reflect.TypeFor[ContinuationMode](), reflect.TypeFor[contract.ContinuationMode]()},
		{"ReadOnlyMechanism", reflect.TypeFor[ReadOnlyMechanism](), reflect.TypeFor[contract.ReadOnlyMechanism]()},
		{"ReadOnlyContainment", reflect.TypeFor[ReadOnlyContainment](), reflect.TypeFor[contract.ReadOnlyContainment]()},
		{"ReadOnlyCapability", reflect.TypeFor[ReadOnlyCapability](), reflect.TypeFor[contract.ReadOnlyCapability]()},
		{"RuntimeCapability", reflect.TypeFor[RuntimeCapability](), reflect.TypeFor[contract.RuntimeCapability]()},
		{"Description", reflect.TypeFor[Description](), reflect.TypeFor[contract.Description]()},
	}
	for _, pair := range pairs {
		if pair.provider != pair.contract {
			t.Errorf("provider.%s is a distinct type from contract.%s", pair.name, pair.name)
		}
	}
	if !errors.Is(ErrProfileUnavailable, contract.ErrProfileUnavailable) {
		t.Fatal("provider.ErrProfileUnavailable is not the contract error value")
	}
}

func TestProviderConstantsKeepTheirPublishedValues(t *testing.T) {
	values := []struct{ got, want string }{
		{OptionInteractive, "interactive"},
		{OptionContinuation, "continuation"},
		{OptionEffort, "effort"},
		{OptionModel, "model"},
		{OptionNativeTimeout, "native-timeout"},
		{OptionReviewTarget, "review-target"},
		{string(ContinuationNative), "native"},
		{string(ContinuationCheckpoint), "checkpoint"},
		{string(ContinuationUnsupported), "unsupported"},
		{string(ReadOnlyNativePlan), "native-plan"},
		{string(ReadOnlyNativeSandbox), "native-sandbox"},
		{string(ReadOnlyToolAllowlist), "tool-allowlist"},
		{string(ReadOnlyProviderOwned), "provider-owned"},
		{ErrProfileUnavailable.Error(), "provider permission profile is not available"},
	}
	for _, value := range values {
		if value.got != value.want {
			t.Errorf("constant = %q, want %q", value.got, value.want)
		}
	}
}

// asContractDescription compiles only while provider.Description and
// contract.Description are one type.
func asContractDescription(description contract.Description) contract.Description {
	return description
}

func TestDescriptionBytesAreIdenticalThroughBothNames(t *testing.T) {
	viaProvider := Description{
		ID: "alpha", Continuation: ContinuationCheckpoint, SupportedOptions: []string{OptionModel},
		ReadOnly: ReadOnlyCapability{Supported: true, Mechanism: ReadOnlyToolAllowlist, Containment: ReadOnlyProviderOwned},
		Runtime:  RuntimeCapability{VersionArgs: []string{"-v"}, RequiredFlags: []string{"--json"}}, Discoverable: true,
	}
	viaContract := asContractDescription(viaProvider)
	left, err := json.Marshal(viaProvider)
	if err != nil {
		t.Fatalf("Marshal(provider.Description) error = %v", err)
	}
	right, err := json.Marshal(viaContract)
	if err != nil {
		t.Fatalf("Marshal(contract.Description) error = %v", err)
	}
	if string(left) != string(right) {
		t.Fatalf("serialized descriptions differ:\nprovider %s\ncontract %s", left, right)
	}
}

type placementPaths struct {
	base, state, work, cache string
}

func newPlacementPaths(t *testing.T) placementPaths {
	t.Helper()
	base, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatalf("CanonicalizePath() error = %v", err)
	}
	paths := placementPaths{base: base, state: filepath.Join(base, "state"), work: filepath.Join(base, "work"), cache: filepath.Join(base, "cache")}
	for _, dir := range []string{paths.state, paths.work, paths.cache} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", dir, err)
		}
	}
	return paths
}

func profileFor(directory string, writable ...string) PreparedProfile {
	return PreparedProfile{Plan: execution.Plan{Directory: directory}, WritableRoots: writable}
}

func TestValidateStatePlacementAcceptsDisjointCanonicalTrees(t *testing.T) {
	paths := newPlacementPaths(t)
	if err := profileFor(paths.work, paths.cache).ValidateStatePlacement(paths.state); err != nil {
		t.Fatalf("ValidateStatePlacement() error = %v, want nil", err)
	}
}

func TestValidateStatePlacementChecksWorkspaceBeforeWritableRoots(t *testing.T) {
	paths := newPlacementPaths(t)
	nested := filepath.Join(paths.state, "cache")
	err := profileFor(nested, "relative").ValidateStatePlacement(paths.state)
	want := fmt.Sprintf("%v: state root overlaps provider-writable tree %s", ErrProfileUnavailable, nested)
	if !errors.Is(err, ErrProfileUnavailable) || err.Error() != want {
		t.Fatalf("error = %v, want workspace overlap %q", err, want)
	}
}

func TestValidateStatePlacementRejectsFilesystemAliases(t *testing.T) {
	paths := newPlacementPaths(t)
	alias := filepath.Join(paths.base, "work-alias")
	if err := os.Symlink(paths.work, alias); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	err := profileFor(paths.work, alias).ValidateStatePlacement(paths.state)
	want := fmt.Sprintf("%v: provider writable root is not canonical: %s", ErrProfileUnavailable, alias)
	if !errors.Is(err, ErrProfileUnavailable) || err.Error() != want {
		t.Fatalf("aliased writable root error = %v, want %q", err, want)
	}
	stateAlias := filepath.Join(paths.base, "state-alias")
	if err = os.Symlink(paths.state, stateAlias); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	err = profileFor(paths.work).ValidateStatePlacement(stateAlias)
	want = fmt.Sprintf("%v: state root is not canonical", ErrProfileUnavailable)
	if !errors.Is(err, ErrProfileUnavailable) || err.Error() != want {
		t.Fatalf("aliased state root error = %v, want %q", err, want)
	}
}

func TestValidateStatePlacementRejectsStateInsideOrAboveWritableTree(t *testing.T) {
	paths := newPlacementPaths(t)
	for _, writable := range []string{paths.state, paths.base} {
		err := profileFor(paths.work, writable).ValidateStatePlacement(paths.state)
		want := fmt.Sprintf("%v: state root overlaps provider-writable tree %s", ErrProfileUnavailable, writable)
		if !errors.Is(err, ErrProfileUnavailable) || err.Error() != want {
			t.Errorf("writable %q error = %v, want %q", writable, err, want)
		}
	}
}
