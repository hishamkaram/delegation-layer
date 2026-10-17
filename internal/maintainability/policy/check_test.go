package policy

import (
	"strings"
	"testing"
)

func testManifest() Manifest {
	return Manifest{Limits: Limits{Code: 800, Tests: 1000, Documentation: 1800, Function: 80}, Modules: []Module{{Name: "go:domain", Owner: "domain", Layer: "pure"}}}
}

func TestPureBoundaryRejectsEffectfulImportsEvenWhenRegistered(t *testing.T) {
	for _, target := range []string{"go:os", "go:unsafe"} {
		t.Run(target, func(t *testing.T) {
			manifest := testManifest()
			manifest.Modules[0].Imports = []string{target}
			findings := Check(manifest, []Source{{Path: "domain/value.go", Module: "go:domain", Imports: []Dependency{{Target: target}}}})
			if len(findings) != 1 || findings[0].Kind != "purity" {
				t.Fatalf("effectful registered import accepted: %+v", findings)
			}
		})
	}
}

func TestPureBoundaryUsesPythonModuleSeparators(t *testing.T) {
	cases := []struct {
		target        string
		wantEffectful bool
	}{
		{target: "py:os.path", wantEffectful: true},
		{target: "py:importlib.util", wantEffectful: true},
		{target: "py:ctypes.util", wantEffectful: true},
		{target: "py:pathlib"},
		{target: "py:datetime"},
		{target: "py:posixpath"},
		{target: "py:typing"},
	}
	for _, test := range cases {
		t.Run(test.target, func(t *testing.T) {
			manifest := Manifest{Limits: testManifest().Limits, Modules: []Module{{
				Name: "py:domain", Owner: "domain", Layer: "pure", Imports: []string{test.target},
			}}}
			source := Source{
				Path: "domain/value.py", Module: "py:domain",
				Imports: []Dependency{{Target: test.target}},
			}
			findings := Check(manifest, []Source{source})
			if test.wantEffectful {
				if len(findings) != 1 || findings[0].Kind != "purity" {
					t.Fatalf("effectful Python import accepted: %+v", findings)
				}
				return
			}
			if len(findings) != 0 {
				t.Fatalf("registered pure standard-library import rejected: %+v", findings)
			}
		})
	}
}

func TestDataImportsAndCorrectedCounterpartPass(t *testing.T) {
	manifest := testManifest()
	manifest.Modules[0].Imports = []string{"go:time", "go:io/fs"}
	source := Source{Path: "domain/value.go", Module: "go:domain", Imports: []Dependency{{Target: "go:time"}, {Target: "go:io/fs"}}}
	if findings := Check(manifest, []Source{source}); len(findings) != 0 {
		t.Fatalf("data-only counterpart rejected: %+v", findings)
	}
}

func TestReadOnlyReflectionIsNotAnEffectfulImport(t *testing.T) {
	manifest := testManifest()
	manifest.Modules[0].Imports = append(manifest.Modules[0].Imports, "go:reflect")
	source := Source{Path: "domain/codec.go", Module: "go:domain", Imports: []Dependency{{Target: "go:reflect"}}}
	if findings := Check(manifest, []Source{source}); len(findings) != 0 {
		t.Fatalf("codec reflection rejected as I/O: %+v", findings)
	}
	source.Effects = []Effect{{Name: "reflection mutation or invocation: Call", Line: 3}}
	if findings := Check(manifest, []Source{source}); len(findings) != 1 || findings[0].Kind != "purity" {
		t.Fatalf("reflection invocation was accepted: %+v", findings)
	}
}

func TestPureLayerCannotAuthorizeAnEffectfulProjectDependency(t *testing.T) {
	manifest := testManifest()
	manifest.Modules[0].Imports = append(manifest.Modules[0].Imports, "go:storage")
	manifest.Modules = append(manifest.Modules, Module{Name: "go:storage", Owner: "storage", Layer: "runtime"})
	sources := []Source{
		{Path: "domain/value.go", Module: "go:domain", Imports: []Dependency{{Target: "go:storage"}}},
		{Path: "storage/value.go", Module: "go:storage"},
	}
	findings := Check(manifest, sources)
	if len(findings) != 1 || !strings.Contains(findings[0].Detail, "non-pure boundary") {
		t.Fatalf("registered outward dependency was accepted: %+v", findings)
	}
	manifest.Modules[1].Layer = "pure"
	if findings = Check(manifest, sources); len(findings) != 0 {
		t.Fatalf("inward pure dependency rejected: %+v", findings)
	}
}

func TestBoundaryRejectsUnknownModuleAndUnregisteredDependency(t *testing.T) {
	cases := []Source{
		{Path: "domain/nested/renamed_test.go", Module: "go:new-package#test"},
		{Path: "domain/native_darwin.go", Module: "go:domain", Imports: []Dependency{{Target: "go:os/exec"}}},
	}
	for _, source := range cases {
		if findings := Check(testManifest(), []Source{source}); len(findings) == 0 {
			t.Fatalf("unregistered boundary accepted: %+v", source)
		}
	}
}

func TestBudgetsIncludeTestsAndDoNotDependOnFunctionName(t *testing.T) {
	for _, name := range []string{"ordinary", "runOrdinary", "TestOrdinary"} {
		source := Source{Path: "fixture/test_values.py", Kind: "test", Lines: 1001, Functions: []Function{{Name: name, Length: 81}}}
		findings := checkSize(testManifest().Limits, source)
		if len(findings) != 2 {
			t.Fatalf("test budget bypass for %s: %+v", name, findings)
		}
		source.Lines, source.Functions[0].Length = 1000, 80
		if got := checkSize(testManifest().Limits, source); len(got) != 0 {
			t.Fatalf("exact boundary rejected: %+v", got)
		}
	}
}

func TestCyclesUseObservedEdges(t *testing.T) {
	manifest := testManifest()
	manifest.Modules = append(manifest.Modules, Module{Name: "go:other", Owner: "other", Layer: "pure", Imports: []string{"go:domain"}})
	manifest.Modules[0].Imports = append(manifest.Modules[0].Imports, "go:other")
	sources := []Source{{Path: "a.go", Module: "go:domain", Imports: []Dependency{{Target: "go:other"}}}, {Path: "b.go", Module: "go:other", Imports: []Dependency{{Target: "go:domain"}}}}
	findings := Check(manifest, sources)
	if len(findings) != 1 || findings[0].Kind != "cycle" || !strings.Contains(findings[0].Detail, "go:domain") {
		t.Fatalf("cycle not diagnosed: %+v", findings)
	}
}

func TestDependencyRegistryRejectsUnusedAllowedEdges(t *testing.T) {
	manifest := Manifest{Modules: []Module{{Name: "go:domain", Owner: "domain", Layer: "pure", Imports: []string{"go:strings", "go:fmt"}}}}
	sources := []Source{{Path: "domain/a.go", Module: "go:domain", Imports: []Dependency{{Target: "go:strings"}}}}
	findings := Check(manifest, sources)
	if len(findings) != 1 || findings[0].Kind != "registry" || !strings.Contains(findings[0].Detail, "go:fmt") {
		t.Fatalf("unused allowed dependency was accepted: %+v", findings)
	}
	sources = append(sources, Source{Path: "domain/b_darwin.go", Module: "go:domain", Imports: []Dependency{{Target: "go:fmt"}}})
	if findings = Check(manifest, sources); len(findings) != 0 {
		t.Fatalf("dependency observed in another source of its module was rejected: %+v", findings)
	}
	manifest.Modules[0].Imports = []string{"go:strings"}
	if findings = Check(manifest, sources[:1]); len(findings) != 0 {
		t.Fatalf("corrected exact registry was rejected: %+v", findings)
	}
}
