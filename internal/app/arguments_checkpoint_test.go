package app

import (
	"strings"
	"testing"
	"time"
)

func TestCheckpointArgumentsAcceptDocumentedPlacements(t *testing.T) {
	id, op := strings.Repeat("a", 32), strings.Repeat("b", 32)
	cases := []struct {
		argv []string
		want Arguments
	}{
		{[]string{"--root", "/state", "checkpoint", "create", "--cwd", "/repo", "--budget", "2m", "--json"}, Arguments{Root: "/state", CheckpointAction: "create", Cwd: "/repo", CheckpointBudget: "2m", JSON: true}},
		{[]string{"checkpoint", "--json", "create", "--workspace", id, "--root=/state"}, Arguments{Root: "/state", CheckpointAction: "create", WorkspaceID: id, JSON: true}},
		{[]string{"--root", "/state", "checkpoint", "status", id, "--operation", op, "--watch", "30s", "--json"}, Arguments{Root: "/state", CheckpointAction: "status", CheckpointID: id, CheckpointOperationID: op, Watch: 30 * time.Second, JSON: true}},
		{[]string{"checkpoint", "status", "--json", "--", id}, Arguments{CheckpointAction: "status", CheckpointID: id, JSON: true}},
		{[]string{"checkpoint", "list", "--after", id, "--limit", "1"}, Arguments{CheckpointAction: "list", CheckpointAfter: id, CheckpointLimit: 1}},
		{[]string{"checkpoint", "list"}, Arguments{CheckpointAction: "list", CheckpointLimit: defaultCheckpointPageSize}},
		{[]string{"checkpoint", "remove", id, "--budget", "1m", "--json"}, Arguments{CheckpointAction: "remove", CheckpointID: id, CheckpointBudget: "1m", JSON: true}},
	}
	for _, tc := range cases {
		got, err := ParseArguments(tc.argv)
		if err != nil {
			t.Fatalf("%v: %v", tc.argv, err)
		}
		tc.want.Command = "checkpoint"
		if got.Command != tc.want.Command || got.Root != tc.want.Root || got.CheckpointAction != tc.want.CheckpointAction ||
			got.Cwd != tc.want.Cwd || got.WorkspaceID != tc.want.WorkspaceID || got.CheckpointID != tc.want.CheckpointID ||
			got.CheckpointOperationID != tc.want.CheckpointOperationID || got.CheckpointAfter != tc.want.CheckpointAfter ||
			got.CheckpointLimit != tc.want.CheckpointLimit || got.CheckpointBudget != tc.want.CheckpointBudget ||
			got.Watch != tc.want.Watch || got.JSON != tc.want.JSON {
			t.Fatalf("%v parsed as %+v, want %+v", tc.argv, got, tc.want)
		}
	}
}

func TestCheckpointArgumentsRejectAmbiguousOrInvalidRequests(t *testing.T) {
	id := strings.Repeat("a", 32)
	cases := map[string][]string{
		"missing action":            {"checkpoint", "--json"},
		"unknown action":            {"checkpoint", "apply", id},
		"no target":                 {"checkpoint", "create", "--json"},
		"both targets":              {"checkpoint", "create", "--cwd", "/repo", "--workspace", id},
		"relative cwd":              {"checkpoint", "create", "--cwd", "repo"},
		"invalid workspace":         {"checkpoint", "create", "--workspace", "ws"},
		"positional target":         {"checkpoint", "create", "/repo"},
		"create watch":              {"checkpoint", "create", "--cwd", "/repo", "--watch", "1s"},
		"zero budget":               {"checkpoint", "create", "--cwd", "/repo", "--budget", "0s"},
		"negative budget":           {"checkpoint", "remove", id, "--budget", "-1s"},
		"infinite budget":           {"checkpoint", "remove", id, "--budget", "forever"},
		"missing status ID":         {"checkpoint", "status", "--json"},
		"invalid status ID":         {"checkpoint", "status", "abc"},
		"extra status ID":           {"checkpoint", "status", id, id},
		"invalid operation":         {"checkpoint", "status", id, "--operation", "op"},
		"negative watch":            {"checkpoint", "status", id, "--watch", "-1ns"},
		"overflow watch":            {"checkpoint", "status", id, "--watch", "999999999999h"},
		"status budget":             {"checkpoint", "status", id, "--budget", "1m"},
		"remove operation":          {"checkpoint", "remove", id, "--operation", id},
		"remove watch":              {"checkpoint", "remove", id, "--watch", "1s"},
		"missing remove ID":         {"checkpoint", "remove"},
		"list positional":           {"checkpoint", "list", id},
		"list cwd":                  {"checkpoint", "list", "--cwd", "/repo"},
		"list zero limit":           {"checkpoint", "list", "--limit", "0"},
		"list large limit":          {"checkpoint", "list", "--limit", "51"},
		"invalid cursor":            {"checkpoint", "list", "--after", "x"},
		"unknown option":            {"checkpoint", "status", id, "--force"},
		"duplicate option":          {"checkpoint", "status", id, "--json", "--json"},
		"relative root":             {"checkpoint", "list", "--root", "state"},
		"supervisor-only global":    {"--runner", "/runner", "checkpoint", "list"},
		"pueue-config not accepted": {"checkpoint", "list", "--pueue-config", "/config"},
	}
	for name, argv := range cases {
		if parsed, err := ParseArguments(argv); err == nil {
			t.Fatalf("%s: accepted %v as %+v", name, argv, parsed)
		}
	}
}
