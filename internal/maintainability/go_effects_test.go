package maintainability

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestGoEffectsSeparateAmbientReferencesFromPureAPIs(t *testing.T) {
	cases := []struct {
		module, member string
		effect         bool
	}{
		{"time", "Since", true},
		{"time", "Until", true},
		{"time", "Parse", false},
		{"io/ioutil", "ReadFile", true},
		{"io/ioutil", "NopCloser", false},
		{"log", "Print", true},
		{"log", "Ldate", false},
		{"runtime", "Caller", true},
		{"runtime", "GC", true},
		{"runtime", "GOOS", false},
		{"path/filepath", "Abs", true},
		{"path/filepath", "Clean", false},
	}
	for _, test := range cases {
		t.Run(test.module+"."+test.member, func(t *testing.T) {
			root := t.TempDir()
			writeCollectorFixture(t, root, "source.go", "package p\nimport value \""+test.module+"\"\nvar observed = value."+test.member+"\n")
			source := policy.Source{Path: "source.go"}
			if err := collectGoFile(root, "example.test", &source); err != nil {
				t.Fatal(err)
			}
			manifest := policy.Manifest{Modules: []policy.Module{{Name: "go:.", Owner: "fixture", Layer: "pure", Imports: []string{"go:" + test.module}}}}
			findings := policy.Check(manifest, []policy.Source{source})
			if test.effect && (len(findings) != 1 || findings[0].Kind != "purity") || !test.effect && len(findings) != 0 {
				t.Fatalf("unexpected purity verdict: %+v", findings)
			}
		})
	}
}

func TestGoEffectsResolveParserBoundLocalShadows(t *testing.T) {
	root := t.TempDir()
	writeCollectorFixture(t, root, "source.go", `package p
import clock "time"
var duration clock.Duration
func supplied(clock struct{ Since func() }) { clock.Since() }
func ambient() { _ = clock.Since }
`)
	source := policy.Source{Path: "source.go"}
	if err := collectGoFile(root, "example.test", &source); err != nil {
		t.Fatal(err)
	}
	if len(source.Effects) != 1 || source.Effects[0].Name != "time.Since" || source.Effects[0].Line != 5 {
		t.Fatalf("local binding confused with imported clock: %+v", source.Effects)
	}
}
