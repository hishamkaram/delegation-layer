package task

import (
	"reflect"
	"testing"
)

func TestSelectEnvironmentPreservesScopeSelection(t *testing.T) {
	base := []string{"SCOPE=caller"}
	cases := []struct {
		name     string
		values   []string
		recorded bool
		want     []string
	}{
		{name: "unrecorded nil retains base", want: base},
		{name: "unrecorded empty retains base", values: []string{}, want: base},
		{name: "recorded nil selects empty", recorded: true, want: []string{}},
		{name: "recorded empty selects empty", values: []string{}, recorded: true, want: []string{}},
		{name: "unmarked values select scope", values: []string{"SCOPE=saved"}, want: []string{"SCOPE=saved"}},
		{name: "recorded values select scope", values: []string{"SCOPE=saved"}, recorded: true, want: []string{"SCOPE=saved"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected, err := SelectEnvironment(base, tc.values, tc.recorded)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(selected, tc.want) {
				t.Fatalf("selected = %#v; want %#v", selected, tc.want)
			}
		})
	}
}

func TestSelectEnvironmentCopiesSelectedInput(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		base := []string{"SCOPE=caller"}
		values := []string(nil)
		if recorded {
			values = []string{"SCOPE=saved"}
		}
		selected, err := SelectEnvironment(base, values, recorded)
		if err != nil {
			t.Fatal(err)
		}
		selected[0] = "SCOPE=changed"
		if base[0] != "SCOPE=caller" || (recorded && values[0] != "SCOPE=saved") {
			t.Fatal("selection aliases its input")
		}
	}
	for _, base := range [][]string{nil, {}} {
		selected, err := SelectEnvironment(base, nil, false)
		if err != nil || selected == nil || len(selected) != 0 {
			t.Fatalf("empty base not materialized: selected %#v, base %#v, error %v", selected, base, err)
		}
	}
}

func TestSelectEnvironmentValidatesOnlySelectedSavedValues(t *testing.T) {
	for _, values := range [][]string{
		{"missing-separator"}, {"=value"}, {"KEY=one", "KEY=two"}, {"K\x00EY=value"}, {"KEY=val\x00ue"},
	} {
		for _, recorded := range []bool{false, true} {
			selected, err := SelectEnvironment([]string{"SCOPE=caller"}, values, recorded)
			if err == nil || selected != nil {
				t.Fatalf("invalid saved values accepted: selected %#v, error %v", selected, err)
			}
		}
	}
	base := []string{"LEGACY", "KEY=one", "KEY=two"}
	selected, err := SelectEnvironment(base, nil, false)
	if err != nil || !reflect.DeepEqual(selected, base) {
		t.Fatalf("unrecorded scope revalidates its parent: selected %#v, error %v", selected, err)
	}
}
