package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type errWriter struct{}

func (e errWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("simulated write failure")
}

func TestRunSuccessHelp(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"empty slice", []string{}},
		{"help command", []string{"help"}},
		{"help long flag", []string{"--help"}},
		{"help short flag", []string{"-h"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}

			code := run(tc.args, stdout, stderr)
			if code != 0 {
				t.Fatalf("expected exit code 0, got %d", code)
			}
			if stdout.String() != usageText {
				t.Fatalf("expected usage text on stdout, got: %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected empty stderr, got: %q", stderr.String())
			}
		})
	}
}

func TestRunSuccessVersion(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"version command", []string{"version"}},
		{"version flag", []string{"--version"}},
	}

	expected := fmt.Sprintf("delegate %s\n", version)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}

			code := run(tc.args, stdout, stderr)
			if code != 0 {
				t.Fatalf("expected exit code 0, got %d", code)
			}
			if stdout.String() != expected {
				t.Fatalf("expected %q on stdout, got: %q", expected, stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected empty stderr, got: %q", stderr.String())
			}
		})
	}
}

func TestRunUnknownCommandOrFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"unknown-command"}},
		{"unknown flag", []string{"--unknown"}},
		{"arbitrary string", []string{"something-else"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}

			code := run(tc.args, stdout, stderr)
			if code != 2 {
				t.Fatalf("expected exit code 2, got %d", code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout, got: %q", stdout.String())
			}
			errMsg := stderr.String()
			expectedErr := fmt.Sprintf("error: unknown command or flag %q", tc.args[0])
			if !strings.Contains(errMsg, expectedErr) {
				t.Fatalf("expected %q in stderr, got: %q", expectedErr, errMsg)
			}
			if !strings.Contains(errMsg, usageText) {
				t.Fatalf("expected usage text in stderr, got: %q", errMsg)
			}
		})
	}
}

func TestRunExtraArguments(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		expectedSub string
	}{
		{"help with extra argument", []string{"help", "extra"}, "unexpected extra argument \"extra\" for help"},
		{"--help with extra argument", []string{"--help", "foo"}, "unexpected extra argument \"foo\" for help"},
		{"version with extra argument", []string{"version", "extra"}, "unexpected extra argument \"extra\" for version"},
		{"--version with extra argument", []string{"--version", "bar"}, "unexpected extra argument \"bar\" for version"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}

			code := run(tc.args, stdout, stderr)
			if code != 2 {
				t.Fatalf("expected exit code 2, got %d", code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout, got: %q", stdout.String())
			}
			errMsg := stderr.String()
			if !strings.Contains(errMsg, tc.expectedSub) {
				t.Fatalf("expected %q in stderr, got: %q", tc.expectedSub, errMsg)
			}
			if !strings.Contains(errMsg, usageText) {
				t.Fatalf("expected usage text in stderr, got: %q", errMsg)
			}
		})
	}
}

func TestRunWriterFailures(t *testing.T) {
	badWriter := errWriter{}
	validBuffer := &bytes.Buffer{}

	t.Run("stdout failure on no args", func(t *testing.T) {
		code := run(nil, badWriter, validBuffer)
		if code != 1 {
			t.Fatalf("expected exit code 1 on stdout write failure, got %d", code)
		}
	})

	t.Run("stdout failure on help", func(t *testing.T) {
		code := run([]string{"help"}, badWriter, validBuffer)
		if code != 1 {
			t.Fatalf("expected exit code 1 on stdout write failure, got %d", code)
		}
	})

	t.Run("stdout failure on version", func(t *testing.T) {
		code := run([]string{"version"}, badWriter, validBuffer)
		if code != 1 {
			t.Fatalf("expected exit code 1 on stdout write failure, got %d", code)
		}
	})

	t.Run("stderr failure on unknown command", func(t *testing.T) {
		code := run([]string{"unknown"}, validBuffer, badWriter)
		if code != 1 {
			t.Fatalf("expected exit code 1 on stderr write failure, got %d", code)
		}
	})

	t.Run("stderr failure on extra args to help", func(t *testing.T) {
		code := run([]string{"help", "extra"}, validBuffer, badWriter)
		if code != 1 {
			t.Fatalf("expected exit code 1 on stderr write failure, got %d", code)
		}
	})

	t.Run("stderr failure on extra args to version", func(t *testing.T) {
		code := run([]string{"version", "extra"}, validBuffer, badWriter)
		if code != 1 {
			t.Fatalf("expected exit code 1 on stderr write failure, got %d", code)
		}
	})
}
