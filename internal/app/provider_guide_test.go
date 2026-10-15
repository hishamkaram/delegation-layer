package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProviderGuideMatchesNativeCatalog(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "providers.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := make(map[string][]string)
	for line := range strings.SplitSeq(string(content), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 7 {
			t.Fatalf("unexpected provider table row: %s", line)
		}
		id := strings.Trim(strings.TrimSpace(cells[1]), "`")
		if _, duplicate := rows[id]; duplicate {
			t.Fatalf("duplicate provider row: %s", id)
		}
		rows[id] = cells
	}
	for _, description := range NativeCatalog().Descriptions() {
		cells, found := rows[description.ID]
		if !found {
			t.Errorf("provider guide missing %s", description.ID)
			continue
		}
		assertProviderGuideSet(t, description.ID, "modes", cells[2], slices.Clone(description.SupportedModes))
		assertProviderGuideSet(t, description.ID, "options", cells[4], slices.Clone(description.SupportedOptions))
		delete(rows, description.ID)
	}
	if len(rows) != 0 {
		t.Errorf("provider guide has extra rows: %v", rows)
	}
}

func assertProviderGuideSet(t *testing.T, id, field, cell string, want []string) {
	t.Helper()
	var got []string
	parts := strings.Split(cell, "`")
	for index := 1; index < len(parts); index += 2 {
		got = append(got, parts[index])
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s %s: guide=%v, catalog=%v", id, field, got, want)
	}
}
