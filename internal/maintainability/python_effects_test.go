package maintainability

import (
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func pythonEffectsFixture(t *testing.T, source string) policy.Source {
	t.Helper()
	root := collectorRoot(t)
	writeCollectorFixture(t, root, "fixture.py", source+"\nraise RuntimeError('inspected module must not execute')\n")
	facts, err := languageFacts(root, "python", []string{"fixture.py"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected one source fact: %+v", facts)
	}
	return facts[0]
}

func requirePythonEffects(t *testing.T, source string, want ...policy.Effect) {
	t.Helper()
	fact := pythonEffectsFixture(t, source)
	if !slices.Equal(fact.Effects, want) {
		t.Fatalf("effects = %+v, want %+v", fact.Effects, want)
	}
}

func TestPythonFactsObserveImportedPathHomeAndWorkingDirectory(t *testing.T) {
	cases := []struct {
		binding string
		call    string
		method  string
	}{
		{"from pathlib import Path", "Path.home()", "home"},
		{"from pathlib import Path as P", "P.cwd()", "cwd"},
		{"import pathlib", "pathlib.Path.home()", "home"},
		{"import pathlib as paths", "paths.Path.cwd()", "cwd"},
		{"from pathlib import Path as P", "P('.').home()", "home"},
	}
	for _, test := range cases {
		t.Run(test.call, func(t *testing.T) {
			requirePythonEffects(t, test.binding+"\n"+test.call,
				policy.Effect{Name: "pathlib.Path." + test.method, Line: 2})
		})
	}
}

func TestPythonFactsObserveDirectImportedPathOperations(t *testing.T) {
	calls := []string{
		"resolve()", "absolute()", "expanduser()", "open()",
		"read_text()", "read_bytes()", "write_text('x')", "write_bytes(b'x')",
		"exists()", "stat()", "lstat()", "iterdir()", "glob('*')", "rglob('*')",
		"is_dir()", "is_file()", "is_symlink()", "is_mount()", "is_socket()",
		"is_fifo()", "is_block_device()", "is_char_device()", "readlink()", "owner()", "group()",
		"mkdir()", "rmdir()", "unlink()", "touch()", "rename('x')", "replace('x')",
		"chmod(0o600)", "lchmod(0o600)", "symlink_to('x')", "hardlink_to('x')", "link_to('x')",
	}
	for _, call := range calls {
		method := call[:strings.IndexByte(call, '(')]
		t.Run(method, func(t *testing.T) {
			for _, binding := range []struct{ statement, constructor string }{
				{"from pathlib import Path as P", "P"},
				{"import pathlib as paths", "paths.Path"},
			} {
				source := binding.statement + "\n" + binding.constructor + "(__file__)." + call
				requirePythonEffects(t, source, policy.Effect{Name: "pathlib.Path." + method, Line: 2})
			}
		})
	}
}

func TestPythonFactsObserveKnownPathResultChains(t *testing.T) {
	requirePythonEffects(t, "from pathlib import Path\nPath(__file__).resolve().parent.absolute()",
		policy.Effect{Name: "pathlib.Path.absolute", Line: 2},
		policy.Effect{Name: "pathlib.Path.resolve", Line: 2})
	requirePythonEffects(t, "from pathlib import Path as P\nP('.').joinpath('child').with_suffix('.py').parent.exists()",
		policy.Effect{Name: "pathlib.Path.exists", Line: 2})
	requirePythonEffects(t, "from pathlib import Path\nPath('.').read_text().exists()",
		policy.Effect{Name: "pathlib.Path.read_text", Line: 2})
	requirePythonEffects(t, "import pathlib as p\np.Path.home().read_text()",
		policy.Effect{Name: "pathlib.Path.read_text", Line: 2},
		policy.Effect{Name: "pathlib.Path.home", Line: 2})
}

func TestPythonFactsObserveImportQualifiedCurrentClocks(t *testing.T) {
	cases := []struct{ binding, call, qualified string }{
		{"import datetime", "datetime.datetime.now()", "datetime.datetime.now"},
		{"import datetime as dates", "dates.datetime.utcnow()", "datetime.datetime.utcnow"},
		{"from datetime import datetime", "datetime.today()", "datetime.datetime.today"},
		{"from datetime import datetime as Clock", "Clock.now(tz=None)", "datetime.datetime.now"},
		{"from datetime import datetime as Clock", "Clock.utcnow()", "datetime.datetime.utcnow"},
		{"import datetime as dates", "dates.date.today()", "datetime.date.today"},
		{"from datetime import date as Day", "Day.today()", "datetime.date.today"},
	}
	for _, test := range cases {
		t.Run(test.call, func(t *testing.T) {
			requirePythonEffects(t, test.binding+"\n"+test.call, policy.Effect{Name: test.qualified, Line: 2})
		})
	}
}

func TestPythonFactsDoNotBanLexicalPathsOrDatetimeParsing(t *testing.T) {
	source := `from pathlib import Path as P
from datetime import datetime as Clock, date as Day
P('/a').is_absolute()
P('/a').as_posix()
P('/a').joinpath('b').with_name('c').with_suffix('.py').relative_to('/a')
P('/a').parts
Clock.strptime('2026-10-16', '%Y-%m-%d')
Clock.fromisoformat('2026-10-16T12:00:00')
Day.fromisoformat('2026-10-16')
Clock(2026, 10, 16).isoformat()
Day(2026, 10, 16).weekday()
P.home
Clock.now
'P.home(); Clock.now(); open("decoy")'
# P.cwd(); Clock.utcnow()
`
	requirePythonEffects(t, source)
}

func TestPythonFactsDoNotTreatUnrelatedMethodNamesAsEffects(t *testing.T) {
	for _, source := range []string{
		"value.resolve(); value.home(); value.now(); value.read_text(); value.exists()",
		"import other_pathlib as paths\npaths.Path(__file__).resolve()",
		"from .pathlib import Path\nPath.home()",
		"from other.datetime import datetime\ndatetime.now()",
		"class Path:\n    def home(self):\n        return '/literal'\nPath().home()",
	} {
		t.Run(source, func(t *testing.T) { requirePythonEffects(t, source) })
	}
}

func TestPythonFactsKeepExistingBuiltinObservations(t *testing.T) {
	source := "open('x')\ninput()\nprint('x')\nexec('x')\neval('x')\n__import__('x')"
	requirePythonEffects(t, source,
		policy.Effect{Name: "open", Line: 1}, policy.Effect{Name: "input", Line: 2},
		policy.Effect{Name: "print", Line: 3}, policy.Effect{Name: "exec", Line: 4},
		policy.Effect{Name: "eval", Line: 5}, policy.Effect{Name: "__import__", Line: 6})
}

func TestPythonFactsDocumentConservativeImportAliasBounds(t *testing.T) {
	// Import spellings are conservative module-wide candidates, not Python name resolution.
	requirePythonEffects(t, "from pathlib import Path as P\ndef use(P):\n    return P.home()",
		policy.Effect{Name: "pathlib.Path.home", Line: 3})
	requirePythonEffects(t, "from pathlib import Path as P\nP = another_factory\nP.cwd()",
		policy.Effect{Name: "pathlib.Path.cwd", Line: 3})
	requirePythonEffects(t, "import datetime as value\nimport custom as value\nvalue.date.today()",
		policy.Effect{Name: "datetime.date.today", Line: 3})
	// Assignment aliases and returned objects outside the finite chain set are not inferred.
	requirePythonEffects(t, "from pathlib import Path\np = Path('.')\np.exists()")
	requirePythonEffects(t, "from datetime import datetime\nclock = datetime.now\nclock()")
}

func TestPythonEffectsFeedTheExistingPolicyChecker(t *testing.T) {
	manifest := policy.Manifest{
		Limits:  policy.Limits{Code: 800, Tests: 1000, Function: 80},
		Modules: []policy.Module{{Name: "py:fixture", Owner: "fixture", Layer: "pure", Imports: []string{"py:pathlib", "py:datetime"}}},
	}
	source := pythonEffectsFixture(t, "from pathlib import Path\nfrom datetime import datetime\nPath.home()\ndatetime.now()")
	findings := policy.Check(manifest, []policy.Source{source})
	if len(findings) != 2 {
		t.Fatalf("pure observations not rejected by canonical checker: %+v", findings)
	}
	for index, want := range []policy.Effect{{Name: "pathlib.Path.home", Line: 3}, {Name: "datetime.datetime.now", Line: 4}} {
		if !slices.ContainsFunc(findings, func(finding policy.Finding) bool {
			return finding.Kind == "purity" && finding.Line == want.Line && finding.Detail == want.Name && finding.Path == source.Path
		}) {
			t.Fatalf("missing effect %d: %+v", index, findings)
		}
	}
	manifest.Modules[0].Layer = "runtime"
	if findings = policy.Check(manifest, []policy.Source{source}); len(findings) != 0 {
		t.Fatalf("runtime observations rejected: %+v", findings)
	}
	manifest.Modules[0].Layer = "pure"
	corrected := pythonEffectsFixture(t, "from pathlib import Path\nfrom datetime import datetime\nPath('/literal').is_absolute()\ndatetime.fromisoformat('2026-10-16T12:00:00')")
	if findings = policy.Check(manifest, []policy.Source{corrected}); len(findings) != 0 {
		t.Fatalf("corrected pure counterpart rejected: %+v", findings)
	}
}
