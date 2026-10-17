package maintainability

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestReflectionGuardCoversSiblingFilesAndMethodValues(t *testing.T) {
	for _, statement := range []string{
		"value(target).SetInt(2)",
		"set := value(target).SetInt; set(2)",
		"value(target).Clear()",
	} {
		root := collectorRoot(t)
		writeCollectorFixture(t, root, "domain/value.go", "package domain\nimport \"reflect\"\nfunc value(target *int)reflect.Value{return reflect.ValueOf(target).Elem()}\n")
		writeCollectorFixture(t, root, "domain/change.go", "package domain\nfunc change(target *int){"+statement+"}\n")
		sources := []policy.Source{{Path: "domain/value.go"}, {Path: "domain/change.go"}}
		if err := collectGo(root, "example.test", sources); err != nil {
			t.Fatal(err)
		}
		if len(sources[0].Effects) != 0 || len(sources[1].Effects) != 1 {
			t.Fatalf("cross-file reflection effect escaped: %+v", sources)
		}
	}
}

func TestReflectionPackageGuardAllowsOnlyReadOnlyMetadata(t *testing.T) {
	for _, name := range []string{"Copy", "Swapper", "MakeFunc", "Select", "New", "Append"} {
		root := collectorRoot(t)
		writeCollectorFixture(t, root, "domain/value.go", "package domain\nimport r \"reflect\"\nvar operation=r."+name+"\n")
		sources := []policy.Source{{Path: "domain/value.go"}}
		if err := collectGo(root, "example.test", sources); err != nil {
			t.Fatal(err)
		}
		if len(sources[0].Effects) != 1 {
			t.Fatalf("reflection package operation %s escaped: %+v", name, sources)
		}
	}
}

func TestReadOnlyReflectionAcrossFilesAndKindsRemainAllowed(t *testing.T) {
	root := collectorRoot(t)
	writeCollectorFixture(t, root, "domain/value.go", "package domain\nimport r \"reflect\"\nvar kind=r.Pointer\nfunc value(target *int)r.Value{return r.ValueOf(target).Elem()}\n")
	writeCollectorFixture(t, root, "domain/read.go", "package domain\nfunc valid(target *int)bool{return value(target).IsValid()}\n")
	sources := []policy.Source{{Path: "domain/value.go"}, {Path: "domain/read.go"}}
	if err := collectGo(root, "example.test", sources); err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if len(source.Effects) != 0 {
			t.Fatalf("read-only metadata rejected: %+v", sources)
		}
	}
}
