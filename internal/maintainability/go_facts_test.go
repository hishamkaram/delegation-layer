package maintainability

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestGoFactsCoverAliasesDirectivesAndBuildExcludedFiles(t *testing.T) {
	cases := []struct {
		name, source, effect string
	}{
		{"alias", "package p\nimport clock \"time\"\nvar now=clock.Now\n", "time.Now"},
		{"dot", "package p\nimport . \"time\"\nvar now=Now\n", "dot import"},
		{"linkname", "package p\nimport _ \"unsafe\"\n//go:linkname now runtime.nanotime\nfunc now()int64\n", "go:linkname directive"},
		{"native", "//go:build darwin\n\npackage p\nimport \"time\"\nvar now=time.Now\n", "time.Now"},
		{"reflection-mutation", "package p\nimport r \"reflect\"\nfunc change(p *int){r.ValueOf(p).Elem().SetInt(1)}\n", "reflection mutation or invocation: SetInt"},
		{"reflection-callback", "package p\nimport r \"reflect\"\nfunc invoke(fn func()){r.ValueOf(fn).Call(nil)}\n", "reflection mutation or invocation: Call"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "value.go")
			if err := os.WriteFile(path, []byte(item.source), 0o600); err != nil {
				t.Fatal(err)
			}
			source := policy.Source{Path: "value.go"}
			if err := collectGoFile(root, "example.test", &source); err != nil {
				t.Fatal(err)
			}
			facts := []policy.Source{source}
			if err := collectModuleReflection(root, facts); err != nil {
				t.Fatal(err)
			}
			source = facts[0]
			if len(source.Effects) != 1 || source.Effects[0].Name != item.effect {
				t.Fatalf("missing static boundary evidence: %+v", source)
			}
		})
	}
}

func TestGoFactsPermitReadOnlyCodecReflection(t *testing.T) {
	root := t.TempDir()
	data := "package p\nimport r \"reflect\"\nfunc fields(target *int) r.Type{return r.TypeOf(target).Elem()}\n"
	if err := os.WriteFile(filepath.Join(root, "codec.go"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	source := policy.Source{Path: "codec.go"}
	if err := collectGoFile(root, "example.test", &source); err != nil || len(source.Effects) != 0 {
		t.Fatalf("deterministic codec reflection rejected: %+v, %v", source, err)
	}
}

func TestGoFactsRejectMalformedSyntaxAndIgnoreStringDecoys(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.go")
	if err := os.WriteFile(path, []byte("package p\nvar sample=`//go:linkname fake\n}\ntime.Now()`\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := policy.Source{Path: "value.go"}
	if err := collectGoFile(root, "example.test", &source); err != nil || len(source.Effects) != 0 {
		t.Fatalf("string decoy was interpreted: %+v, %v", source, err)
	}
	if err := os.WriteFile(path, []byte("package p\nfunc broken("), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collectGoFile(root, "example.test", &source); err == nil {
		t.Fatal("malformed source accepted")
	}
}
