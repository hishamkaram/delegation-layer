package claude

import (
	"testing"
)

func TestQuotaAdviceUsesOnlyAttributedBoundedReset(t *testing.T) {
	for _, tc := range []struct{ name, info, want string }{
		{"valid", `{"status":"rejected","resetsAt":1794380400}`, "2026-11-11T07:00:00Z"},
		{"missing", `{"status":"rejected"}`, ""},
		{"quoted", `{"status":"rejected","resetsAt":"1794380400"}`, ""},
		{"fractional", `{"status":"rejected","resetsAt":1794380400.5}`, ""},
		{"overflow", `{"status":"rejected","resetsAt":99999999999999999999999}`, ""},
		{"duplicate", `{"status":"rejected","resetsAt":1794380400,"resetsAt":1794380500}`, ""},
		{"allowed", `{"status":"allowed","resetsAt":1794380400}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := evaluateQuotaTrace(t, NewWorkspaceWriteInterpreter(), []string{writeInitLine(claudeTestUUID), rateLimitInfoLine(tc.info), apiErrorResult(`,"result_index":0`)}, 1)
			reset := ""
			if got.Quota != nil {
				reset = got.Quota.NotBefore
			}
			if reset != tc.want {
				t.Fatalf("reset=%q want=%q", reset, tc.want)
			}
		})
	}
}
