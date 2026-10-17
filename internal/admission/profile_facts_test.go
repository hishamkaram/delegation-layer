package admission

import (
	"encoding/json"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestInspectionFactsCompareCanonicalObjectsBeforeFinalize(t *testing.T) {
	if !inspectionFactsMatch(json.RawMessage(`{"z":2,"a":1}`), json.RawMessage(`{"a":1,"z":2}`)) {
		t.Fatal("equivalent canonical facts differed only by object field order")
	}
	if inspectionFactsMatch(json.RawMessage(`{"eligible":true}`), json.RawMessage(`{"eligible":false}`)) {
		t.Fatal("changed inspection facts were accepted")
	}
	if inspectionFactsMatch(json.RawMessage(`{"eligible":true}`), json.RawMessage(`not-json`)) {
		t.Fatal("malformed fresh facts were accepted")
	}
	runtime := commonprovider.RuntimeFacts{Executable: "/bin/provider", Version: "provider-1", SHA256: strings.Repeat("a", 64)}
	stored, err := commonprovider.EncodeInspectionFacts(runtime, json.RawMessage(`{"eligible":true}`))
	if err != nil {
		t.Fatal(err)
	}
	changedRuntime := runtime
	changedRuntime.Executable = "/opt/provider-new"
	changedRuntime.Version = "provider-2"
	changedRuntime.SHA256 = strings.Repeat("b", 64)
	fresh, err := commonprovider.EncodeInspectionFacts(changedRuntime, json.RawMessage(`{"eligible":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !inspectionFactsMatch(stored, fresh) {
		t.Fatal("runtime identity drift changed the native inspection contract")
	}
	changedNative, err := commonprovider.EncodeInspectionFacts(changedRuntime, json.RawMessage(`{"eligible":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if inspectionFactsMatch(stored, changedNative) {
		t.Fatal("native inspection drift was accepted with runtime identity drift")
	}
	nativeWithRuntimeKey := json.RawMessage(`{"runtime":{"provider":"native"},"eligible":true}`)
	if !inspectionFactsMatchForDefinition(nativeWithRuntimeKey, nativeWithRuntimeKey, false) {
		t.Fatal("native-only projection with a runtime key was decoded as an envelope")
	}
}
