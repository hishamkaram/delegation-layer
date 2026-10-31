package task

import "testing"

const savedCwdMessage = "saved task cwd must be a clean absolute path"

// canonicalCwdCases is the independent oracle for the saved-cwd policy: a path
// is canonical when it is absolute, lexically clean, and nonblank without NUL.
// Literal spaces inside an otherwise valid path are part of the path, and the
// policy does not inspect the filesystem or require UTF-8.
func canonicalCwdCases(t *testing.T) []struct {
	name  string
	value string
	valid bool
} {
	t.Helper()
	return []struct {
		name  string
		value string
		valid bool
	}{
		{name: "native temp dir", value: t.TempDir(), valid: true},
		{name: "root", value: "/", valid: true},
		{name: "fixture workspace", value: "/fixture/workspace", valid: true},
		{name: "interior space", value: "/work space/project", valid: true},
		{name: "trailing space is literal", value: "/project ", valid: true},
		{name: "leading space in a component is literal", value: "/ project", valid: true},
		{name: "non UTF-8 bytes are preserved", value: "/native/\xff\xfe", valid: true},
		{name: "empty", value: "", valid: false},
		{name: "blank", value: "  ", valid: false},
		{name: "tab only", value: "\t", valid: false},
		{name: "relative", value: "workspace", valid: false},
		{name: "dot relative", value: "./workspace", valid: false},
		{name: "leading space makes it relative", value: " /workspace", valid: false},
		{name: "parent segment", value: "/work/../space", valid: false},
		{name: "current segment", value: "/work/./space", valid: false},
		{name: "doubled separator", value: "/work//space", valid: false},
		{name: "trailing separator", value: "/work/", valid: false},
		{name: "parent of root", value: "/..", valid: false},
		{name: "NUL suffix", value: "/work\x00", valid: false},
		{name: "NUL only", value: "\x00", valid: false},
	}
}

func TestValidateTaskConfigurationSavedCwdPolicy(t *testing.T) {
	for _, tc := range canonicalCwdCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			record := fixtureTask(t)
			record.CanonicalCwd = tc.value
			err := validateTaskConfiguration(&record)
			switch {
			case tc.valid && err != nil:
				t.Fatalf("canonical cwd %q rejected: %v", tc.value, err)
			case !tc.valid && (err == nil || err.Error() != savedCwdMessage):
				t.Fatalf("cwd %q error = %v, want %q", tc.value, err, savedCwdMessage)
			}
		})
	}
}

func TestIsCanonicalCwd(t *testing.T) {
	for _, tc := range canonicalCwdCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCanonicalCwd(tc.value); got != tc.valid {
				t.Fatalf("IsCanonicalCwd(%q) = %t, want %t", tc.value, got, tc.valid)
			}
		})
	}
}

func TestValidateTaskConfigurationReportsCwdAfterEarlierFaults(t *testing.T) {
	invalidPrior := func(r *TaskRecord) {
		r.PriorSession = &PriorSession{Provider: r.Provider, ConversationID: "known", PredecessorTaskID: r.TaskID}
	}
	cases := []struct {
		name   string
		mutate func(*TaskRecord)
		want   string
	}{
		{name: "provider before cwd", mutate: func(r *TaskRecord) { r.Provider = "Bad"; r.CanonicalCwd = "relative" }, want: `unsupported provider: "Bad"`},
		{name: "requested config before cwd", mutate: func(r *TaskRecord) { r.Mode = "bogus"; r.CanonicalCwd = "relative" }, want: "requested permission must match the explicit task mode"},
		{name: "native timeout before cwd", mutate: func(r *TaskRecord) { r.RequestedConfig.NativeTimeout = "1000ms"; r.CanonicalCwd = "relative" }, want: "native timeout must be normalized"},
		{name: "cwd before prior session", mutate: func(r *TaskRecord) { invalidPrior(r); r.CanonicalCwd = "relative" }, want: savedCwdMessage},
		{name: "prior session after valid cwd", mutate: invalidPrior, want: "invalid prior session binding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := fixtureTask(t)
			tc.mutate(&record)
			if err := validateTaskConfiguration(&record); err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSavedCwdPolicyDoesNotTouchTheFilesystem(t *testing.T) {
	missing := t.TempDir() + "/never/created"
	record := fixtureTask(t)
	record.CanonicalCwd = missing
	if err := validateTaskConfiguration(&record); err != nil {
		t.Fatalf("lexically canonical but absent cwd rejected: %v", err)
	}
}
