package maintainability

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestPythonEffectsRejectAmbientStdlibReferences(t *testing.T) {
	cases := []struct{ module, member string }{
		{"shutil", "copyfile"},
		{"tempfile", "TemporaryDirectory"},
		{"fcntl", "flock"},
		{"signal", "signal"},
		{"threading", "Thread"},
		{"sys", "stdout"},
		{"sys", "argv"},
		{"secrets", "token_hex"},
		{"uuid", "uuid4"},
		{"uuid", "getnode"},
	}
	for _, test := range cases {
		t.Run(test.module+"."+test.member, func(t *testing.T) {
			for _, source := range []string{
				"import " + test.module + " as ambient\nvalue = ambient." + test.member,
				"from " + test.module + " import " + test.member + " as ambient\nvalue = ambient",
			} {
				requirePythonEffects(t, source, policy.Effect{Name: test.module + "." + test.member, Line: 2})
			}
		})
	}
}

func TestPythonEffectsKeepDeterministicStdlibAPIs(t *testing.T) {
	requirePythonEffects(t, `import uuid
from secrets import compare_digest
from signal import SIGTERM
import sys
uuid.UUID('12345678-1234-5678-1234-567812345678')
uuid.uuid5(uuid.NAMESPACE_DNS, 'example.test')
compare_digest('one', 'two')
SIGTERM
sys.version_info
`)
}

func TestPythonStdlibEffectsReachPurePolicy(t *testing.T) {
	source := pythonEffectsFixture(t, "from uuid import uuid4 as random_id\nrandom_id()")
	manifest := policy.Manifest{Limits: policy.Limits{Function: 80}, Modules: []policy.Module{{Name: "py:fixture", Owner: "fixture", Layer: "pure", Imports: []string{"py:uuid"}}}}
	findings := policy.Check(manifest, []policy.Source{source})
	if len(findings) != 1 || findings[0].Kind != "purity" {
		t.Fatalf("effectful UUID factory passed pure policy: %+v", findings)
	}
}
