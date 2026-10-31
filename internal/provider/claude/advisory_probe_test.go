package claude

import (
	"os"
	"reflect"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// baselineAdmissionCapability is the pending runtime capability digest that
// the baseline release (0b5a725) recorded when it admitted a print task under
// its then-current reference, now the frozen advisory revision. That release
// probed only the stable --print flag. The oracle is built from that literal
// flag set through the shared provider constructor, never through this
// package's reference-to-probe mapping.
func baselineAdmissionCapability(t *testing.T, request task.TaskRecord, environment []string) string {
	t.Helper()
	cli, err := commonprovider.LocateCLI("claude", os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, environment, commonprovider.RuntimeCapability{RequiredFlags: []string{"--print"}})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := commonprovider.RuntimeCapabilityDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	return capability
}

// TestFrozenAdvisoryPrintRecordsKeepTheirAdmittedProbe reconstructs a pending
// print task admitted by the baseline release under the reference that is now
// the frozen advisory revision. It must rebuild the exact runtime probe that
// admission recorded, in both modes, so an unchanged CLI still matches the
// recorded capability digest.
func TestFrozenAdvisoryPrintRecordsKeepTheirAdmittedProbe(t *testing.T) {
	directory := fixtureClaudeEnvironment(t)
	baselineDigests := map[string]string{Mode: advisoryReadDigest, WorkspaceWriteMode: advisoryWriteDigest}
	for _, mode := range []string{Mode, WorkspaceWriteMode} {
		request := profileRequest()
		request.CanonicalCwd, request.Mode, request.RequestedConfig.Permission = directory, mode, mode
		reference := advisoryReference(mode)
		if reference.SHA256 != baselineDigests[mode] {
			t.Fatalf("%s advisory reference %s is not the baseline admission reference", mode, reference.SHA256)
		}
		// The launch scope and native policy an admission records do not depend
		// on the reference; only the capability digest comes from the oracle.
		fresh, err := PrepareCandidate(request, os.Environ())
		if err != nil {
			t.Fatal(err)
		}
		admitted, err := fresh.Admit()
		if err != nil {
			t.Fatal(err)
		}
		meta := task.MetaRecord{
			Predicate: reference, EffectiveConfig: admitted.Effective, Environment: admitted.Plan.Environment, EnvironmentRecorded: true,
			RuntimePreflightSHA256: baselineAdmissionCapability(t, request, admitted.Plan.Environment),
		}
		existing, err := PrepareExistingCandidate(request, meta, os.Environ())
		if err != nil {
			t.Fatalf("%s reconstruction: %v", mode, err)
		}
		if flags := existing.Inspection.Runtime.RequiredFlags; !reflect.DeepEqual(flags, []string{"--print"}) {
			t.Errorf("%s %s reconstructed probe flags=%q, want the admitted --print", mode, reference.Version, flags)
		}
		capability, err := commonprovider.RuntimeCapabilityDigest(*existing.Inspection)
		if err != nil || capability != meta.RuntimePreflightSHA256 || !reflect.DeepEqual(existing.Inspection.Environment, meta.Environment) {
			t.Errorf("%s %s reconstructed capability=%s err=%v, want the admitted %s", mode, reference.Version, capability, err, meta.RuntimePreflightSHA256)
		}
	}
}
