package sourceinventory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type formatRecipeCase struct {
	name            string
	empty           bool
	inventoryExit   int
	temporaryExit   int
	formatterExit   int
	wantFormatter   bool
	wantMakeFailure bool
}

func TestMakeFormattingUsesCanonicalInventory(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	targets := []struct{ name, flag string }{{"fmt", "-w"}, {"fmt-check", "-l"}}
	cases := []formatRecipeCase{
		{name: "paths", wantFormatter: true},
		{name: "empty_inventory", empty: true},
		{name: "temporary_file_failure", temporaryExit: 29, wantMakeFailure: true},
		{name: "inventory_failure", inventoryExit: 31, wantMakeFailure: true},
		{name: "formatter_failure", formatterExit: 23, wantFormatter: true, wantMakeFailure: true},
	}
	for _, target := range targets {
		for _, scenario := range cases {
			t.Run(target.name+"/"+scenario.name, func(t *testing.T) {
				fixture := newFormatMakeFixture(t, makefile)
				paths := fixture.inputs(t)
				if scenario.empty {
					paths = nil
				}
				output, runErr := fixture.run(t, target.name, scenario, paths)
				requireFormatMakeOutcome(t, output, runErr, scenario.wantMakeFailure)
				var wantGo []string
				if scenario.temporaryExit == 0 {
					wantGo = []string{"invocation", "run", "./cmd/source-inventory", "--kind", "go", "--allow-empty"}
				}
				requireFormatNULLog(t, fixture.root, "go.calls.nul", wantGo)
				var want []string
				if scenario.wantFormatter {
					want = append([]string{"invocation", target.flag}, paths...)
				}
				requireFormatNULLog(t, fixture.root, "formatter.calls.nul", want)
			})
		}
	}
}

type formatMakeFixture struct {
	root string
}

func newFormatMakeFixture(t *testing.T, makefile []byte) formatMakeFixture {
	t.Helper()
	fixture := formatMakeFixture{root: filepath.Join(t.TempDir(), "fixture root")}
	writeFormatFile(t, fixture.root, "Makefile", makefile, 0o600)
	writeFormatFile(t, fixture.root, "bin/go", []byte(`#!/bin/bash
set -euo pipefail
printf '%s\0' invocation "$@" >> "$FORMAT_TEST_GO_LOG"
cat "$FORMAT_TEST_INVENTORY"
exit "$FORMAT_TEST_INVENTORY_EXIT"
`), 0o700)
	writeFormatFile(t, fixture.root, "bin/gofumpt", []byte(`#!/bin/bash
set -euo pipefail
printf '%s\0' invocation "$@" >> "$FORMAT_TEST_FORMATTER_LOG"
exit "$FORMAT_TEST_FORMATTER_EXIT"
`), 0o700)
	return fixture
}

func (fixture formatMakeFixture) inputs(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, name := range []string{"kept.go", "with space.go", "with\nnewline.go"} {
		writeFormatFile(t, fixture.root, name, []byte("package fixture\n"), 0o600)
		paths = append(paths, filepath.Join(fixture.root, name))
	}
	writeFormatFile(t, fixture.root, ".local/unlisted.go", []byte("package ignored\n"), 0o600)
	return paths
}

func (fixture formatMakeFixture) run(
	t *testing.T, target string, scenario formatRecipeCase, paths []string,
) (string, error) {
	t.Helper()
	stream := strings.Join(paths, "\x00")
	if len(paths) > 0 {
		stream += "\x00"
	}
	writeFormatFile(t, fixture.root, "inventory.nul", []byte(stream), 0o600)
	if scenario.temporaryExit != 0 {
		writeFormatFile(t, fixture.root, "bin/mktemp",
			[]byte("#!/bin/bash\nexit "+strconv.Itoa(scenario.temporaryExit)+"\n"), 0o700)
	}
	command := exec.Command("make", "--no-print-directory", "-o", "tool-versions", ".SHELLFLAGS=-c", target)
	command.Dir = fixture.root
	command.Env = append(command.Environ(),
		"PATH="+filepath.Join(fixture.root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FORMAT_TEST_INVENTORY="+filepath.Join(fixture.root, "inventory.nul"),
		"FORMAT_TEST_GO_LOG="+filepath.Join(fixture.root, "go.calls.nul"),
		"FORMAT_TEST_INVENTORY_EXIT="+strconv.Itoa(scenario.inventoryExit),
		"FORMAT_TEST_FORMATTER_LOG="+filepath.Join(fixture.root, "formatter.calls.nul"),
		"FORMAT_TEST_FORMATTER_EXIT="+strconv.Itoa(scenario.formatterExit),
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

func requireFormatMakeOutcome(t *testing.T, output string, err error, wantFailure bool) {
	t.Helper()
	if !wantFailure {
		if err != nil {
			t.Fatalf("make unexpectedly failed: %v\n%s", err, output)
		}
		return
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
		t.Fatalf("make did not propagate command failure: %v\n%s", err, output)
	}
}

func requireFormatNULLog(t *testing.T, root, name string, want []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if errors.Is(err, os.ErrNotExist) {
		if len(want) != 0 {
			t.Fatalf("missing %s, want invocation %q", name, want)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != 0 {
		t.Fatalf("%s is not a nonempty NUL-terminated invocation log: %q", name, data)
	}
	actual := strings.Split(string(data[:len(data)-1]), "\x00")
	if !slices.Equal(actual, want) {
		t.Fatalf("%s = %q, want %q", name, actual, want)
	}
}
