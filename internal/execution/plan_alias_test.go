package execution

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution/plan"
)

// The conversion helpers compile only while execution.Plan and plan.Plan are
// one type.
func asPurePlan(value plan.Plan) plan.Plan { return value }

func asExecutionPlan(value Plan) Plan { return value }

func TestPlanIsTheSerializablePlanType(t *testing.T) {
	if reflect.TypeOf(Plan{}) != reflect.TypeOf(plan.Plan{}) {
		t.Fatal("execution.Plan and plan.Plan are distinct types")
	}
	pure := asPurePlan(Plan{Executable: "/bin/provider", Directory: "/work"})
	if pure.Executable != "/bin/provider" || pure.Directory != "/work" {
		t.Fatalf("plan.Plan lost fields assigned through execution.Plan: %+v", pure)
	}
	viaAlias := asExecutionPlan(plan.Plan{Arguments: []string{"-"}})
	if !reflect.DeepEqual(viaAlias.Arguments, []string{"-"}) {
		t.Fatalf("execution.Plan lost fields assigned through plan.Plan: %+v", viaAlias)
	}
}

func TestPlanBytesAreIdenticalThroughEitherName(t *testing.T) {
	viaExecution, err := json.Marshal(Plan{Executable: "/bin/provider", Arguments: []string{"a", ""}, Directory: "/work"})
	if err != nil {
		t.Fatalf("Marshal(execution.Plan) error = %v", err)
	}
	viaPlan, err := json.Marshal(plan.Plan{Executable: "/bin/provider", Arguments: []string{"a", ""}, Directory: "/work"})
	if err != nil {
		t.Fatalf("Marshal(plan.Plan) error = %v", err)
	}
	if string(viaExecution) != string(viaPlan) {
		t.Fatalf("serialized plans differ:\nexecution %s\nplan      %s", viaExecution, viaPlan)
	}
	const want = `{"Executable":"/bin/provider","ExecutableSHA256":"","RuntimePreflightSHA256":"","ProviderRuntime":`
	if got := string(viaExecution); !strings.HasPrefix(got, want) {
		t.Fatalf("serialized plan prefix changed: %s", got)
	}
}
