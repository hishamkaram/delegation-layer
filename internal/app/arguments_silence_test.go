package app

import (
	"strings"
	"testing"
)

func TestOutputSilenceArgumentsPreservePolicy(t *testing.T) {
	for _, command := range []string{"dispatch", "continue"} {
		for _, value := range []string{"", "0", "250ms", "2h"} {
			t.Run(command+"/"+value, func(t *testing.T) {
				args := []string{command}
				if command == "dispatch" {
					args = append(args, "--provider", "claude:print", "--brief", "brief.md", "--cwd", "/workspace")
				} else {
					args = append(args, "--task", strings.Repeat("a", 32))
				}
				if value != "" {
					args = append(args, "--output-silence-timeout", value)
				}
				result, err := ParseArguments(args)
				if err != nil {
					t.Fatal(err)
				}
				if result.Config.OutputSilenceTimeout != value {
					t.Fatalf("silence policy changed: got %q want %q", result.Config.OutputSilenceTimeout, value)
				}
			})
		}
	}
}

func TestOutputSilenceArgumentsRejectInvalidDuration(t *testing.T) {
	for _, value := range []string{"-1s", "forever", "99999999999999999999999999h"} {
		_, err := ParseArguments([]string{"continue", "--task", strings.Repeat("a", 32), "--output-silence-timeout", value})
		if err == nil {
			t.Fatalf("accepted invalid silence duration %q", value)
		}
	}
}
