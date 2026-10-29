package contract

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func optionsRequest(config task.TaskConfig) Request {
	return Request{Auto: true, CanonicalCwd: "/work", Config: config}
}

func TestRequestStoresSuppliedOptionsExactlyAsGiven(t *testing.T) {
	accepted := map[string]task.TaskConfig{
		"omitted":                          {},
		"workspace write":                  {Permission: policy.ModeWorkspaceWrite},
		"unnormalized budget":              {Budget: "90m"},
		"timeout within budget":            {Budget: "30m", NativeTimeout: "30m"},
		"timeout without budget":           {NativeTimeout: "45m"},
		"literal model and effort":         {Model: "provider/model-2.5", Effort: "xhigh"},
		"uncommitted review target":        {ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetUncommitted}},
		"commit review target":             {ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetCommit, Value: "abc123"}},
		"unicode model":                    {Model: "modèle-π"},
		"html-significant review base ref": {ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "<a>&b"}},
	}
	for name, config := range accepted {
		request := optionsRequest(config)
		encoded, err := MarshalRequest(request)
		if err != nil {
			t.Errorf("%s rejected: %v", name, err)
			continue
		}
		decoded, err := DecodeRequest(encoded)
		if err != nil || !reflect.DeepEqual(decoded, request) {
			t.Errorf("%s: stored %+v err=%v, want the supplied request unchanged", name, decoded, err)
		}
	}
}

func TestRequestRejectsInvalidSuppliedOptions(t *testing.T) {
	cases := []struct {
		name   string
		config task.TaskConfig
		is     error
	}{
		{name: "unknown permission", config: task.TaskConfig{Permission: "admin"}, is: policy.ErrInvalidMode},
		{name: "unparsable budget", config: task.TaskConfig{Budget: "soon"}, is: policy.ErrInvalidBudget},
		{name: "nonpositive budget", config: task.TaskConfig{Budget: "0s"}, is: policy.ErrInvalidBudget},
		{name: "unparsable native timeout", config: task.TaskConfig{NativeTimeout: "later"}},
		{name: "nonpositive native timeout", config: task.TaskConfig{NativeTimeout: "-1m"}},
		{name: "native timeout over budget", config: task.TaskConfig{Budget: "30m", NativeTimeout: "31m"}},
		{name: "padded model", config: task.TaskConfig{Model: " gpt"}},
		{name: "model with NUL", config: task.TaskConfig{Model: "gpt\x00"}},
		{name: "padded effort", config: task.TaskConfig{Effort: "high\n"}},
		{name: "unknown review target", config: task.TaskConfig{ReviewTarget: &task.ReviewTarget{Kind: "branch", Value: "main"}}},
		{name: "uncommitted target value", config: task.TaskConfig{ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetUncommitted, Value: "x"}}},
		{name: "empty base target", config: task.TaskConfig{ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetBase}}},
	}
	for _, testCase := range cases {
		request := optionsRequest(testCase.config)
		_, err := MarshalRequest(request)
		if err == nil || (testCase.is != nil && !errors.Is(err, testCase.is)) {
			t.Errorf("%s: err=%v, want rejection identifying %v", testCase.name, err, testCase.is)
		}
		definition := testDefinition()
		definition.Request = request
		if err := ValidateDefinition(definition); err == nil {
			t.Errorf("%s: definition accepted", testCase.name)
		}
	}
}

// encoding/json replaces invalid UTF-8 with U+FFFD, so such a value would be
// stored, digested, and later replayed as a different string than supplied.
func TestRequestRejectsStringsThatCanonicalJSONWouldCoerce(t *testing.T) {
	invalid := "\xff"
	cases := map[string]Request{
		"cwd":    {Auto: true, CanonicalCwd: "/work/" + invalid},
		"model":  optionsRequest(task.TaskConfig{Model: "gpt" + invalid}),
		"effort": optionsRequest(task.TaskConfig{Effort: invalid + "high"}),
	}
	for name, request := range cases {
		if encoded, err := MarshalRequest(request); err == nil {
			t.Errorf("%s: invalid UTF-8 accepted and stored as %q", name, encoded)
		}
		definition := testDefinition()
		definition.Request = request
		if _, err := DefinitionDigests(definition); err == nil {
			t.Errorf("%s: invalid UTF-8 digested", name)
		}
	}
}
