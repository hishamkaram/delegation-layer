package maintainability

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestGoLengthIncludesUncompiledSourceAtPinnedBoundary(t *testing.T) {
	for _, build := range []string{"ignore", "!linux && !darwin", "darwin"} {
		for _, lines := range []int{80, 81} {
			t.Run(build+"/"+strconv.Itoa(lines), func(t *testing.T) {
				root := t.TempDir()
				data := "//go:build " + build + "\n\npackage p\nfunc F() {\n" + strings.Repeat("// counted comment\n", lines) + "}\n"
				writeCollectorFixture(t, root, "excluded_test.go", data)
				sources := []policy.Source{{Path: "excluded_test.go", Kind: "test"}}
				if err := collectGo(root, "example.test", sources); err != nil {
					t.Fatal(err)
				}
				manifest := policy.Manifest{Limits: policy.Limits{Tests: 1000, Function: 80}, Modules: []policy.Module{{Name: "go:.#test", Owner: "fixture", Layer: "test"}}}
				findings := policy.Check(manifest, sources)
				if sources[0].Functions[0].Length != lines {
					t.Fatalf("uncompiled declaration was not measured: %+v", sources[0].Functions)
				}
				if lines == 80 && len(findings) != 0 || lines == 81 && (len(findings) != 1 || findings[0].Kind != "function") {
					t.Fatalf("incorrect boundary verdict: %+v", findings)
				}
			})
		}
	}
}

func TestGoLengthMatchesPinnedFunlen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "module")
	writeCollectorFixture(t, root, "go.mod", "module example.test\n\ngo 1.27.0\n")
	var sources []policy.Source
	cases := []string{
		"func F() {}\n", "func F() {\n}\n",
		"func F() {\n" + strings.Repeat("// comment\n", 80) + "}\n",
		"func F() {\n" + strings.Repeat("// comment\n", 81) + "}\n",
		"func F(\nvalue int,\n) {\n" + strings.Repeat("\n", 79) + "}\n",
		"func F() {\n_ = `\n" + strings.Repeat("raw text\n", 79) + "`\n}\n",
		"func F() {\n_ = func() {\n" + strings.Repeat("// nested comment\n", 79) + "}\n}\n",
	}
	for index, body := range cases {
		for _, suffix := range []string{".go", "_test.go"} {
			path := "case" + strconv.Itoa(index) + strings.ReplaceAll(suffix, ".go", "") + "/value" + suffix
			writeCollectorFixture(t, root, path, "package fixture\n"+body)
			source := policy.Source{Path: path}
			if err := collectGoFile(root, "example.test", &source); err != nil {
				t.Fatal(err)
			}
			sources = append(sources, source)
		}
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "source")
	if err = os.Symlink(root, linked); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, root string }{{"direct", root}, {"symlink", linked}} {
		t.Run(test.name, func(t *testing.T) {
			findings, err := collectFunlen(test.root, filepath.Join(repository, "bin", "golangci-lint"), filepath.Join(repository, ".golangci.yml"), sources)
			if err != nil {
				t.Fatal(err)
			}
			assertPinnedFunlenFindings(t, sources, findings)
		})
	}
}

func assertPinnedFunlenFindings(t *testing.T, sources []policy.Source, findings []policy.Finding) {
	t.Helper()
	for _, source := range sources {
		function := source.Functions[0]
		matched := slices.ContainsFunc(findings, func(finding policy.Finding) bool { return finding.Path == source.Path })
		if matched != (function.Length > 80) {
			t.Fatalf("pinned linter disagrees with collected measurement for %s: %+v, %+v", source.Path, function, findings)
		}
		for _, finding := range findings {
			if finding.Path == source.Path && !strings.Contains(finding.Detail, "("+strconv.Itoa(function.Length)+" > 80)") {
				t.Fatalf("pinned measurement drift: %+v, %+v", function, finding)
			}
		}
	}
}
