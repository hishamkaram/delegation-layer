package control

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlOwnerValidatesInstalledRoleBeforeConsume(t *testing.T) {
	for _, name := range []string{"missing installed", "fixed historical copy", "digest historical copy"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			owner := filepath.Join(root, "missing-owner")
			if name == "fixed historical copy" {
				owner = filepath.Join(root, ".supervisor", "delegate-run")
			}
			if name == "digest historical copy" {
				owner = filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("a", 64))
			}
			command := exec.Command("/bin/sh", "-c", "exit 0")
			command.Dir, command.Env = root, []string{}
			consumed := false
			pending := startControlOwnedCapturedWithConsume(t.Context(), owner, command, strings.Repeat("b", 32), nil, "", func() error { consumed = true; return nil }, nil)
			result, complete := pending.Result()
			if !complete || result.Started || consumed {
				t.Fatalf("unavailable or historical owner consumed command: complete=%t started=%t consumed=%t err=%v", complete, result.Started, consumed, result.Err)
			}
			if name == "missing installed" && !errors.Is(result.Err, os.ErrNotExist) {
				t.Fatalf("installed metadata fault lost original cause: %v", result.Err)
			}
			if name != "missing installed" && !errors.Is(result.Err, ErrConfiguration) {
				t.Fatalf("historical owner did not refuse explicitly: %v", result.Err)
			}
		})
	}
}
