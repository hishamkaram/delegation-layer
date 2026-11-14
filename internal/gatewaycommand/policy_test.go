package gatewaycommand

import (
	"flag"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer-gateway/deviceclient"
	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/provider"
)

func policyDescriptions() []provider.Description {
	return []provider.Description{
		{ID: "claude:print", Discoverable: true, SupportedModes: []string{"read-only", "workspace-write"}, SupportedOptions: []string{"model", "effort"}},
		{ID: "codex:review", Discoverable: true, SupportedModes: []string{"read-only"}, RequiredOptions: []string{"review-target"}},
		{ID: "historical", Discoverable: false, SupportedModes: []string{"read-only"}},
	}
}

func TestGatewayWorkspacePolicyPatchCannotResetOmittedRestrictions(t *testing.T) {
	base := []deviceclient.ProviderPolicy{{Provider: "claude:print", Models: []string{"approved-model"}, Efforts: []string{"high"}, Permissions: []string{"read-only"}, MaxBudgetSeconds: 120, NativeModel: false, NativeEffort: false}}
	a := Arguments{MaximumBudget: 60 * time.Second, budgetSet: true}
	got, err := patchProviderPolicies(a, base, policyDescriptions())
	if err != nil {
		t.Fatal(err)
	}
	want := base[0]
	want.MaxBudgetSeconds = 60
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("omitted restrictions changed: %+v", got)
	}
	got[0].Models[0] = "changed copy"
	if base[0].Models[0] != "approved-model" || base[0].MaxBudgetSeconds != 120 {
		t.Fatal("SDK-owned input policy mutated")
	}
	a = Arguments{Provider: "claude:print", Model: "replacement", modelSet: true}
	got, err = patchProviderPolicies(a, base, policyDescriptions())
	if err != nil || len(got) != 1 || got[0].NativeModel || got[0].Models[0] != "replacement" || got[0].Efforts[0] != "high" || got[0].MaxBudgetSeconds != 120 {
		t.Fatalf("explicit model patch broadened other fields: %+v %v", got, err)
	}
}

func TestGatewayWorkspaceCannotAdvertiseMissingRequiredNativeDefault(t *testing.T) {
	d := provider.Description{ID: "required:model", Discoverable: true, SupportedModes: []string{"read-only"}, SupportedOptions: []string{"model"}, RequiredOptions: []string{"model"}}
	base := []deviceclient.ProviderPolicy{{Provider: d.ID, Models: []string{"required-value"}, Permissions: []string{"read-only"}, MaxBudgetSeconds: 60}}
	if _, err := patchProviderPolicies(Arguments{Provider: d.ID, modelSet: true}, base, []provider.Description{d}); err == nil {
		t.Fatal("required model advertised a native default")
	}
}

func TestGatewayProviderDefaultIsCompiledReadOnlyCeiling(t *testing.T) {
	a := Arguments{Action: "connect"}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	RegisterFlags(fs, &a)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	policies, err := providerPolicies(a, policyDescriptions())
	if err != nil || len(policies) != 1 {
		t.Fatalf("default policies: %+v %v", policies, err)
	}
	p := policies[0]
	if p.Provider != "claude:print" || len(p.Permissions) != 1 || p.Permissions[0] != "read-only" || p.MaxBudgetSeconds != 60 || !p.NativeModel || !p.NativeEffort || len(p.Models) != 0 || len(p.Efforts) != 0 {
		t.Fatalf("unapproved default ceiling: %+v", p)
	}
}

func TestGatewayProviderExplicitSelectionCannotFallback(t *testing.T) {
	a := Arguments{Provider: "claude:print", Model: "exact-model", Effort: "high", Permission: config.ModeReadOnly, MaximumBudget: 60 * time.Second}
	policies, err := providerPolicies(a, policyDescriptions())
	if err != nil || len(policies) != 1 {
		t.Fatalf("explicit policy: %+v %v", policies, err)
	}
	p := policies[0]
	if p.NativeModel || p.NativeEffort || len(p.Models) != 1 || p.Models[0] != "exact-model" || len(p.Efforts) != 1 || p.Efforts[0] != "high" {
		t.Fatalf("explicit native selections broadened: %+v", p)
	}
	for _, id := range []string{"absent", "codex:review", "historical"} {
		a.Provider = id
		if _, err = providerPolicies(a, policyDescriptions()); err == nil {
			t.Fatalf("provider %q silently substituted", id)
		}
	}
}

func TestGatewayProviderBudgetRejectsFractionalAndUnboundedValues(t *testing.T) {
	for _, budget := range []time.Duration{0, -time.Second, time.Millisecond, time.Duration(1<<32) * time.Second} {
		if _, err := providerPolicies(Arguments{Permission: config.ModeReadOnly, MaximumBudget: budget}, policyDescriptions()); err == nil {
			t.Fatalf("budget %s accepted", budget)
		}
	}
}
