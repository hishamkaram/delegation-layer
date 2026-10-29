package task

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

// The expectations below were recorded from the concrete configuration path
// before the supplied-option validator was extracted. They pin which of several
// simultaneous faults surfaces first and the exact message and sentinel
// identity, so sharing leaf validators cannot silently reorder or reword them.

type errorOrderCase struct {
	name   string
	record TaskRecord
	want   string
	is     error
}

func TestNormalizeRequestedConfigMultiFaultErrorOrder(t *testing.T) {
	const padded = " padded "
	badTarget := &ReviewTarget{Kind: "bogus"}
	cases := []errorOrderCase{
		{
			name: "budget before provider", record: TaskRecord{Provider: "Bad", Mode: "bogus", RequestedConfig: TaskConfig{Budget: "nope", NativeTimeout: "nope", Model: padded, ReviewTarget: badTarget}},
			want: `budget must be a finite positive duration: time: invalid duration "nope"`, is: config.ErrInvalidBudget,
		},
		{
			name: "provider before timeout", record: TaskRecord{Provider: "Bad", Mode: "bogus", RequestedConfig: TaskConfig{Budget: "2m", NativeTimeout: "nope", Model: padded, ReviewTarget: badTarget}},
			want: `unsupported provider: "Bad"`, is: config.ErrUnsupportedProvider,
		},
		{
			name: "provider after valid options", record: TaskRecord{Provider: "Bad", RequestedConfig: TaskConfig{Budget: "2m"}},
			want: `unsupported provider: "Bad"`, is: config.ErrUnsupportedProvider,
		},
		{
			name: "budget nanos mismatch before timeout", record: TaskRecord{Provider: config.ProviderCodexExec, BudgetNanos: 1, RequestedConfig: TaskConfig{Budget: "2m", NativeTimeout: "nope", Model: padded}},
			want: "requested budget does not match budget_nanos",
		},
		{
			name: "timeout before mode", record: TaskRecord{Provider: config.ProviderCodexExec, Mode: "bogus", RequestedConfig: TaskConfig{Budget: "2m", NativeTimeout: "3m", Model: padded}},
			want: "native timeout must be positive and no greater than the task budget",
		},
		{
			name: "mode before model", record: TaskRecord{Provider: config.ProviderCodexExec, Mode: "bogus", RequestedConfig: TaskConfig{Budget: "2m", Model: padded, Effort: padded, ReviewTarget: badTarget}},
			want: `invalid permission mode: must be 'read-only' or 'workspace-write': "bogus"`, is: config.ErrInvalidMode,
		},
		{
			name: "mode mismatch before model", record: TaskRecord{Provider: config.ProviderCodexExec, Mode: config.ModeReadOnly, RequestedConfig: TaskConfig{Permission: config.ModeWorkspaceWrite, Model: padded}},
			want: "requested permission must match the explicit task mode",
		},
		{
			name: "model before effort", record: TaskRecord{Provider: config.ProviderCodexExec, RequestedConfig: TaskConfig{Model: "a\x00", Effort: padded, ReviewTarget: badTarget}},
			want: "model and effort must be nonblank literal values when supplied",
		},
		{
			name: "effort before review target", record: TaskRecord{Provider: config.ProviderCodexExec, RequestedConfig: TaskConfig{Effort: padded, ReviewTarget: badTarget}},
			want: "model and effort must be nonblank literal values when supplied",
		},
		{
			name: "review target last", record: TaskRecord{Provider: config.ProviderCodexExec, RequestedConfig: TaskConfig{ReviewTarget: &ReviewTarget{Kind: ReviewTargetBase, Value: " "}}},
			want: "review target value must be a nonblank literal",
		},
		{
			name: "codex review default budget bounds timeout", record: TaskRecord{Provider: config.ProviderCodexReview, RequestedConfig: TaskConfig{NativeTimeout: "2h", Model: padded}},
			want: "native timeout must be positive and no greater than the task budget",
		},
		{
			name: "zero budget before timeout", record: TaskRecord{Provider: config.ProviderCodexExec, RequestedConfig: TaskConfig{Budget: "0s", NativeTimeout: "nope"}},
			want: "budget must be a finite positive duration", is: config.ErrInvalidBudget,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := tc.record
			assertErrorOrder(t, NormalizeRequestedConfig(&record), tc)
		})
	}
}

func TestValidateTaskConfigurationMultiFaultErrorOrder(t *testing.T) {
	const padded = " padded "
	saved := func(requested TaskConfig) TaskRecord {
		requested.Permission = config.ModeWorkspaceWrite
		return TaskRecord{Provider: config.ProviderAntigravityPrint, Mode: config.ModeWorkspaceWrite, BudgetNanos: 120e9, CanonicalCwd: "/workspace", RequestedConfig: requested}
	}
	relativeCwd := saved(TaskConfig{Budget: "2m0s", NativeTimeout: "3000ms"})
	relativeCwd.CanonicalCwd = "relative"
	cases := []errorOrderCase{
		{
			name: "noncanonical budget before model", record: saved(TaskConfig{Budget: "120s", Model: padded}),
			want: "requested budget must be normalized and match budget_nanos",
		},
		{
			name: "model before noncanonical timeout", record: saved(TaskConfig{Budget: "2m0s", Model: padded, NativeTimeout: "3000ms"}),
			want: "model and effort must be nonblank literal values when supplied",
		},
		{
			name: "review target before noncanonical timeout", record: saved(TaskConfig{Budget: "2m0s", NativeTimeout: "3000ms", ReviewTarget: &ReviewTarget{Kind: "bogus"}}),
			want: `unknown review target "bogus"`,
		},
		{
			name: "noncanonical timeout before cwd", record: relativeCwd,
			want: "native timeout must be normalized",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := tc.record
			assertErrorOrder(t, validateTaskConfiguration(&record), tc)
		})
	}
}

func assertErrorOrder(t *testing.T, err error, tc errorOrderCase) {
	t.Helper()
	if err == nil || err.Error() != tc.want {
		t.Fatalf("error = %v, want %q", err, tc.want)
	}
	if tc.is != nil && !errors.Is(err, tc.is) {
		t.Fatalf("error %v lost sentinel %v", err, tc.is)
	}
}
