package command

import (
	"flag"
	"strings"
	"testing"
)

type request struct {
	Root, Name string
	JSON       bool
}

func fixtureEngine() Engine[request] {
	register := func(fs *flag.FlagSet, p *request) error {
		fs.StringVar(&p.Root, "root", "", "absolute root")
		fs.StringVar(&p.Name, "name", "", "literal name")
		fs.BoolVar(&p.JSON, "json", false, "JSON output")
		return nil
	}
	return Engine[request]{
		Root: Spec[request]{Register: func(fs *flag.FlagSet, p *request) error {
			fs.StringVar(&p.Root, "root", "", "absolute root")
			return nil
		}, Children: []Spec[request]{
			{Name: "help", Summary: "Help"}, {Name: "version", Summary: "Version"}, {Name: "group", Children: []Spec[request]{{Name: "show", Summary: "Show one record", Operands: "ID"}}},
		}},
		DefaultRegister: register,
		Validate: func(_ *request, fs *flag.FlagSet) error {
			if len(fs.Args()) != 1 {
				return flag.ErrHelp
			}
			return nil
		},
	}
}

func TestTypedSyntaxKeepsLiteralValuesAndInterspersedFlags(t *testing.T) {
	e := fixtureEngine()
	result, err := e.Parse([]string{"-root", "/literal root", "group", "--name", "--help", "show", "ID", "-json"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Help || result.State.Name != "--help" || result.State.Root != "/literal root" || !result.State.JSON {
		t.Fatalf("literal request changed: %+v", result)
	}
}

func TestRegisteredFlagsAreHelpAndValidationSource(t *testing.T) {
	e := fixtureEngine()
	text, err := e.Help([]string{"group", "show"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Usage: delegate group show ID", "--name", "literal name"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q: %s", want, text)
		}
	}
	for _, argv := range [][]string{{"group", "show", "ID", "--name", "a", "--name=b"}, {"group", "show", "ID", "--unknown"}, {"group", "show", "--name"}} {
		if _, err = e.Parse(argv); err == nil {
			t.Errorf("invalid syntax accepted: %v", argv)
		}
	}
	result, err := e.Parse([]string{"group", "show", "--help"})
	if err != nil || !result.Help || strings.Join(result.HelpPath, " ") != "group show" {
		t.Fatalf("help ran semantic validation: %+v %v", result, err)
	}
}

func TestHelpOnlyConsumesSyntaxBeforeSeparator(t *testing.T) {
	e := fixtureEngine()
	result, err := e.Parse([]string{"group", "show", "--", "--help"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Help {
		t.Fatal("separator payload interpreted as help")
	}
	result, err = e.Parse([]string{"help", "group", "show"})
	if err != nil || !result.Help {
		t.Fatalf("explicit path help failed: %+v %v", result, err)
	}
}

func TestHelpDoesNotFabricateIncompleteExamples(t *testing.T) {
	text, err := fixtureEngine().Help([]string{"group", "show"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "Example:") {
		t.Fatalf("missing metadata fabricated an incomplete example: %s", text)
	}
}

func TestTypedHelpExamplesQuoteLiteralShellValues(t *testing.T) {
	args := []string{"delegate", "command", "--name", "space value", "$(private)", "literal'quote", "", "plain", "*"}
	want := `delegate command --name 'space value' '$(private)' 'literal'"'"'quote' '' plain '*'`
	if got := renderExample(args); got != want {
		t.Fatalf("example changed argv/shell meaning: %s", got)
	}
}
