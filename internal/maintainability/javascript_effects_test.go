package maintainability

import (
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func javascriptEffectsFixture(t *testing.T, source string) policy.Source {
	t.Helper()
	root := collectorRoot(t)
	writeCollectorFixture(t, root, "fixture.mjs", source+"\nthrow Error('must not evaluate inspected source');\n")
	facts, err := languageFacts(root, "javascript", []string{"fixture.mjs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected one source: %+v", facts)
	}
	return facts[0]
}

func TestJavaScriptAmbientReferencesReachPurePolicy(t *testing.T) {
	cases := []struct{ code, effect string }{
		{"export const value = process.env;", "process"},
		{"export const value = Date.now;", "Date.now"},
		{"export const value = new Date();", "Date clock"},
		{"export const value = Date('ignored');", "Date clock"},
		{"export const value = Math.random;", "Math.random"},
		{"export const value = Math['random']();", "Math.random"},
		{"export const value = globalThis.process.cwd();", "process"},
		{"export const value = `${Math.random()}`;", "Math.random"},
		{"import {resolve as locate} from 'node:path'; export const value = locate('relative');", "node:path.resolve"},
		{"import * as paths from 'path'; export const value = paths.resolve;", "node:path.resolve"},
		{"import paths from 'node:path'; export const value = paths.posix.resolve('relative');", "node:path.resolve"},
	}
	for _, test := range cases {
		t.Run(test.effect+"/"+test.code, func(t *testing.T) {
			source := javascriptEffectsFixture(t, test.code)
			module := policy.Module{Name: "js:fixture.mjs", Owner: "fixture", Layer: "pure"}
			for _, dependency := range source.Imports {
				module.Imports = append(module.Imports, dependency.Target)
			}
			findings := policy.Check(policy.Manifest{Modules: []policy.Module{module}}, []policy.Source{source})
			if !slices.ContainsFunc(findings, func(finding policy.Finding) bool { return finding.Kind == "purity" && finding.Detail == test.effect }) {
				t.Fatalf("ambient reference escaped policy: %+v", findings)
			}
		})
	}
}

func TestJavaScriptBuiltinImportAliasesReachPurePolicy(t *testing.T) {
	cases := []struct {
		specifier string
		effectful bool
	}{
		{specifier: "fs", effectful: true},
		{specifier: "node:fs", effectful: true},
		{specifier: "fs/promises", effectful: true},
		{specifier: "node:fs/promises", effectful: true},
		{specifier: "child_process", effectful: true},
		{specifier: "node:child_process", effectful: true},
		{specifier: "http", effectful: true},
		{specifier: "node:http", effectful: true},
		{specifier: "https", effectful: true},
		{specifier: "node:https", effectful: true},
		{specifier: "net", effectful: true},
		{specifier: "node:net", effectful: true},
		{specifier: "vm", effectful: true},
		{specifier: "node:vm", effectful: true},
		{specifier: "path"},
		{specifier: "node:path"},
		{specifier: "path/posix"},
		{specifier: "node:path/win32"},
		{specifier: "fs-extra"},
		{specifier: "net-extra"},
		{specifier: "@scope/net"},
		{specifier: "@scope/fs"},
	}
	for _, test := range cases {
		t.Run(test.specifier, func(t *testing.T) {
			source := javascriptEffectsFixture(t, "import * as value from '"+test.specifier+"'; export {value};")
			target := "js:" + test.specifier
			if !slices.Equal(source.Imports, []policy.Dependency{{Target: target}}) {
				t.Fatalf("collector changed the actual import: %+v", source.Imports)
			}
			module := policy.Module{Name: source.Module, Owner: "fixture", Layer: "pure", Imports: []string{target}}
			findings := policy.Check(policy.Manifest{Modules: []policy.Module{module}}, []policy.Source{source})
			var want []policy.Finding
			if test.effectful {
				want = []policy.Finding{{Path: source.Path, Kind: "purity", Detail: "effectful import " + target}}
			}
			if !slices.Equal(findings, want) {
				t.Fatalf("registered import %s: got %+v, want %+v", target, findings, want)
			}
		})
	}
}

func TestJavaScriptPureAPIsAndLiteralDecoysRemainValid(t *testing.T) {
	source := javascriptEffectsFixture(t, `import {join, normalize, isAbsolute} from 'node:path';
export const parsed = Date.parse('2026-10-17');
export const date = new Date('2026-10-17');
export const utc = Date.UTC(2026, 9, 17);
export const path = normalize(join('/absolute', 'child'));
export const absolute = isAbsolute(path);
export const number = Math.floor(2.5);
export const text = 'process.cwd(); Math.random(); new Date()';
export const expression = /process[.]cwd\(\); Math.random\(\)/;
export const object = {process: 'a name', Date: 'another name'};
export const field = object.process;
// process.cwd(); Date.now(); Math.random()
/* Date() */
`)
	if len(source.Effects) != 0 {
		t.Fatalf("pure APIs or literal decoys rejected: %+v", source.Effects)
	}
}

func TestJavaScriptReferenceSyntaxVariants(t *testing.T) {
	cases := []struct{ source, effect string }{
		{`export const value = new Date;`, "Date clock"},
		{`export const value = Date.now.call(null);`, "Date.now"},
		{`export const value = Math.random.bind(Math);`, "Math.random"},
		{`export const value = Math["rand\u006fm"]();`, "Math.random"},
		{`export const value = pr\u006fcess.cwd();`, "process"},
		{`export const value = globalThis['process'].env;`, "process"},
		{`import paths from 'node:path/posix'; export const value = paths.resolve('.');`, "node:path.resolve"},
		{`import {resolve as base} from 'path/win32'; export const value = base.bind(null);`, "node:path.resolve"},
		{`export const value = 1 / Math.random() / 2;`, "Math.random"},
	}
	for _, test := range cases {
		source := javascriptEffectsFixture(t, test.source)
		if !slices.ContainsFunc(source.Effects, func(effect policy.Effect) bool { return effect.Name == test.effect }) {
			t.Fatalf("reference variant not observed: %s %+v", test.source, source.Effects)
		}
	}
}

func TestJavaScriptCollectorDocumentsConservativeAliasBounds(t *testing.T) {
	source := javascriptEffectsFixture(t, "import {resolve as locate} from 'node:path';\nexport function f(locate) { return locate('x'); }")
	if !slices.Equal(source.Effects, []policy.Effect{{Name: "node:path.resolve", Line: 2}}) {
		t.Fatalf("expected documented conservative import candidate: %+v", source.Effects)
	}
	source = javascriptEffectsFixture(t, "export const object = {resolve(value) { return value; }};\nobject.resolve('x');")
	if len(source.Effects) != 0 {
		t.Fatalf("unrelated method name confused with imported path resolver: %+v", source.Effects)
	}
}
