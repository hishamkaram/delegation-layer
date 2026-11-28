package task

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/config/policy"
)

const (
	timeoutBoundsMessage = "native timeout must be positive and no greater than the task budget"
	modelEffortMessage   = "model and effort must be nonblank literal values when supplied"
	invalidBudgetMessage = `budget must be a finite positive duration or "unlimited"`
)

func TestValidateRequestedOptionsAcceptsOmittedAndLiteralValues(t *testing.T) {
	cases := []struct {
		name   string
		config TaskConfig
	}{
		{name: "nothing supplied"},
		{name: "interactive is not checked here", config: TaskConfig{Interactive: true}},
		{name: "read only", config: TaskConfig{Permission: config.ModeReadOnly}},
		{name: "workspace write", config: TaskConfig{Permission: config.ModeWorkspaceWrite}},
		{name: "budget alone", config: TaskConfig{Budget: "90m"}},
		{name: "unlimited budget", config: TaskConfig{Budget: "unlimited"}},
		{name: "unlimited budget encloses any timeout", config: TaskConfig{Budget: "unlimited", NativeTimeout: "1000h"}},
		{name: "timeout without budget", config: TaskConfig{NativeTimeout: "1s"}},
		{name: "timeout below budget", config: TaskConfig{Budget: "2m", NativeTimeout: "119s"}},
		{name: "timeout equals budget", config: TaskConfig{Budget: "2m", NativeTimeout: "2m"}},
		{name: "equal durations spelled differently", config: TaskConfig{Budget: "120s", NativeTimeout: "2000ms"}},
		{name: "unknown model and effort are not an allowlist", config: TaskConfig{Model: "vendor/any-model-9", Effort: "ludicrous"}},
		{name: "uncommitted review", config: TaskConfig{ReviewTarget: &ReviewTarget{Kind: ReviewTargetUncommitted}}},
		{name: "base review", config: TaskConfig{ReviewTarget: &ReviewTarget{Kind: ReviewTargetBase, Value: "main"}}},
		{name: "commit review", config: TaskConfig{ReviewTarget: &ReviewTarget{Kind: ReviewTargetCommit, Value: "0b5a725"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := cloneTaskConfig(tc.config)
			if err := ValidateRequestedOptions(tc.config); err != nil {
				t.Fatalf("valid supplied options rejected: %v", err)
			}
			assertTaskConfigUnchanged(t, tc.config, before)
		})
	}
}

func TestValidateRequestedOptionsRejectsInvalidSuppliedValues(t *testing.T) {
	cases := []struct {
		name   string
		config TaskConfig
		want   string
		is     error
	}{
		{name: "unknown permission", config: TaskConfig{Permission: "bogus"}, want: `invalid permission mode: must be 'read-only' or 'workspace-write': "bogus"`, is: config.ErrInvalidMode},
		{name: "malformed budget", config: TaskConfig{Budget: "soon"}, want: `budget must be a finite positive duration or "unlimited": time: invalid duration "soon"`, is: config.ErrInvalidBudget},
		{name: "zero budget", config: TaskConfig{Budget: "0s"}, want: invalidBudgetMessage, is: config.ErrInvalidBudget},
		{name: "negative budget", config: TaskConfig{Budget: "-1m"}, want: invalidBudgetMessage, is: config.ErrInvalidBudget},
		{name: "unlimited spelled with case", config: TaskConfig{Budget: "Unlimited"}, want: `budget must be a finite positive duration or "unlimited": time: invalid duration "Unlimited"`, is: config.ErrInvalidBudget},
		{name: "padded unlimited", config: TaskConfig{Budget: " unlimited"}, want: `budget must be a finite positive duration or "unlimited": time: invalid duration " unlimited"`, is: config.ErrInvalidBudget},
		{name: "malformed timeout without budget", config: TaskConfig{NativeTimeout: "soon"}, want: timeoutBoundsMessage},
		{name: "zero timeout without budget", config: TaskConfig{NativeTimeout: "0s"}, want: timeoutBoundsMessage},
		{name: "negative timeout without budget", config: TaskConfig{NativeTimeout: "-1s"}, want: timeoutBoundsMessage},
		{name: "malformed timeout with budget", config: TaskConfig{Budget: "2m", NativeTimeout: "soon"}, want: timeoutBoundsMessage},
		{name: "zero timeout with budget", config: TaskConfig{Budget: "2m", NativeTimeout: "0s"}, want: timeoutBoundsMessage},
		{name: "negative timeout with budget", config: TaskConfig{Budget: "2m", NativeTimeout: "-1s"}, want: timeoutBoundsMessage},
		{name: "overflowing timeout", config: TaskConfig{NativeTimeout: "999999999999999999999999h"}, want: timeoutBoundsMessage},
		{name: "timeout above budget", config: TaskConfig{Budget: "2m", NativeTimeout: "121s"}, want: timeoutBoundsMessage},
		{name: "padded model", config: TaskConfig{Model: " gpt"}, want: modelEffortMessage},
		{name: "model with NUL", config: TaskConfig{Model: "gpt\x00"}, want: modelEffortMessage},
		{name: "padded effort", config: TaskConfig{Effort: "high\n"}, want: modelEffortMessage},
		{name: "effort with NUL", config: TaskConfig{Effort: "\x00"}, want: modelEffortMessage},
		{name: "unknown review kind", config: TaskConfig{ReviewTarget: &ReviewTarget{Kind: "bogus"}}, want: `unknown review target "bogus"`},
		{name: "uncommitted review with value", config: TaskConfig{ReviewTarget: &ReviewTarget{Kind: ReviewTargetUncommitted, Value: "x"}}, want: "uncommitted review target cannot have a value"},
		{name: "blank base review", config: TaskConfig{ReviewTarget: &ReviewTarget{Kind: ReviewTargetBase, Value: " "}}, want: "review target value must be a nonblank literal"},
		{name: "padded commit review", config: TaskConfig{ReviewTarget: &ReviewTarget{Kind: ReviewTargetCommit, Value: "abc "}}, want: "review target value must be a nonblank literal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := cloneTaskConfig(tc.config)
			err := ValidateRequestedOptions(tc.config)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("error %v lost sentinel %v", err, tc.is)
			}
			assertTaskConfigUnchanged(t, tc.config, before)
			// Concrete normalization requires a budget; supply one only where
			// the case omits it so the case's own fault is compared.
			concrete := TaskRecord{Provider: config.ProviderCodexExec, RequestedConfig: cloneTaskConfig(tc.config)}
			if concrete.RequestedConfig.Budget == "" {
				concrete.RequestedConfig.Budget = "2m"
			}
			if concreteErr := NormalizeRequestedConfig(&concrete); concreteErr == nil || concreteErr.Error() != tc.want {
				t.Fatalf("concrete normalization disagrees with the supplied-option check: %v, want %q", concreteErr, tc.want)
			}
		})
	}
}

func TestValidateRequestedOptionsReportsFirstFaultInSyntaxOrder(t *testing.T) {
	const padded = " padded "
	request := TaskConfig{
		Permission:    "bogus",
		Budget:        "soon",
		NativeTimeout: "soon",
		Model:         padded,
		Effort:        padded,
		ReviewTarget:  &ReviewTarget{Kind: "bogus"},
	}
	steps := []struct {
		name string
		want func(error) bool
		fix  func(*TaskConfig)
	}{
		{name: "permission", want: func(err error) bool { return errors.Is(err, config.ErrInvalidMode) }, fix: func(c *TaskConfig) { c.Permission = "" }},
		{name: "budget", want: func(err error) bool { return errors.Is(err, config.ErrInvalidBudget) }, fix: func(c *TaskConfig) { c.Budget = "" }},
		{name: "timeout", want: func(err error) bool { return err != nil && err.Error() == timeoutBoundsMessage }, fix: func(c *TaskConfig) { c.NativeTimeout = "" }},
		{name: "model and effort", want: func(err error) bool { return err != nil && err.Error() == modelEffortMessage }, fix: func(c *TaskConfig) { c.Model, c.Effort = "", "" }},
		{name: "review target", want: func(err error) bool { return err != nil && err.Error() == `unknown review target "bogus"` }, fix: func(c *TaskConfig) { c.ReviewTarget = nil }},
	}
	for _, step := range steps {
		if err := ValidateRequestedOptions(request); !step.want(err) {
			t.Fatalf("%s fault did not surface first: %v", step.name, err)
		}
		step.fix(&request)
	}
	if err := ValidateRequestedOptions(request); err != nil {
		t.Fatalf("fully corrected request rejected: %v", err)
	}
}

func TestValidateRequestedOptionsLeavesDefaultsToConcreteNormalization(t *testing.T) {
	t.Run("omitted fields stay empty for the scheduler to preserve", func(t *testing.T) {
		request := TaskConfig{}
		if err := ValidateRequestedOptions(request); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(request, TaskConfig{}) {
			t.Fatalf("validation supplied a default: %+v", request)
		}
	})
	t.Run("concrete normalization has no provider default budget", requireExplicitBudgetForConcreteNormalization)
	t.Run("semantic equality is accepted but concrete normalization stays canonical", requireConcreteNormalizationStaysCanonical)
}

// Supplied-option validation accepts an omitted budget, but concrete
// normalization of a new request applies no provider default, including the
// former one-hour Codex review default, and leaves the request unchanged.
func requireExplicitBudgetForConcreteNormalization(t *testing.T) {
	request := TaskConfig{NativeTimeout: "40m"}
	if err := ValidateRequestedOptions(request); err != nil {
		t.Fatalf("budget-free timeout rejected before the caller supplies a budget: %v", err)
	}
	for _, provider := range []string{config.ProviderCodexExec, config.ProviderCodexReview} {
		record := TaskRecord{Provider: provider, RequestedConfig: request}
		before := record
		if err := NormalizeRequestedConfig(&record); !errors.Is(err, policy.ErrBudgetRequired) {
			t.Fatalf("%s omitted budget error = %v, want ErrBudgetRequired", provider, err)
		}
		if !reflect.DeepEqual(record, before) {
			t.Fatalf("%s failed normalization supplied a default: %+v", provider, record)
		}
	}
	unlimited := TaskRecord{Provider: config.ProviderCodexReview, RequestedConfig: TaskConfig{Budget: "unlimited", NativeTimeout: "40m"}}
	if err := NormalizeRequestedConfig(&unlimited); err != nil {
		t.Fatalf("explicit unlimited budget rejected: %v", err)
	}
	if unlimited.RequestedConfig.Budget != "unlimited" || unlimited.BudgetNanos != 0 || unlimited.RequestedConfig.NativeTimeout != "40m0s" {
		t.Fatalf("explicit unlimited budget not retained: %+v", unlimited)
	}
}

func requireConcreteNormalizationStaysCanonical(t *testing.T) {
	request := TaskConfig{Budget: "120s", NativeTimeout: "3000ms"}
	if err := ValidateRequestedOptions(request); err != nil {
		t.Fatal(err)
	}
	record := TaskRecord{Provider: config.ProviderAntigravityPrint, Mode: config.ModeWorkspaceWrite, CanonicalCwd: "/workspace", RequestedConfig: request}
	if err := NormalizeRequestedConfig(&record); err != nil {
		t.Fatal(err)
	}
	if record.RequestedConfig.Budget != "2m0s" || record.RequestedConfig.NativeTimeout != "3s" {
		t.Fatalf("concrete normalization changed: %+v", record.RequestedConfig)
	}
	saved := record
	saved.RequestedConfig.Budget = request.Budget
	if err := validateTaskConfiguration(&saved); err == nil {
		t.Fatal("noncanonical saved budget accepted")
	}
}

func cloneTaskConfig(c TaskConfig) TaskConfig {
	if c.ReviewTarget != nil {
		target := *c.ReviewTarget
		c.ReviewTarget = &target
	}
	return c
}

func assertTaskConfigUnchanged(t *testing.T, got, want TaskConfig) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("validation mutated its input: %+v, want %+v", got, want)
	}
}
