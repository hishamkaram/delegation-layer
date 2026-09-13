package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time via -ldflags "-X main.version=...".
// This build-metadata variable is an explicitly documented exception to the no mutable globals rule.
var version = "dev"

const usageText = `Usage: delegate [command|flag]

A durable single-machine agent delegation layer.

Commands:
  help       Show this help message
  version    Print version information

Flags:
  -h, --help     Show this help message
  --version      Print version information

Note: Task orchestration commands (dispatch, status, collect, cancel, logs)
are planned for future phases and not yet implemented in Phase 0.
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		if _, err := fmt.Fprint(stdout, usageText); err != nil {
			return 1
		}
		return 0
	}

	switch args[0] {
	case "help", "--help", "-h":
		if len(args) > 1 {
			if _, err := fmt.Fprintf(stderr, "error: unexpected extra argument %q for help\n\n%s", args[1], usageText); err != nil {
				return 1
			}
			return 2
		}
		if _, err := fmt.Fprint(stdout, usageText); err != nil {
			return 1
		}
		return 0

	case "version", "--version":
		if len(args) > 1 {
			if _, err := fmt.Fprintf(stderr, "error: unexpected extra argument %q for version\n\n%s", args[1], usageText); err != nil {
				return 1
			}
			return 2
		}
		if _, err := fmt.Fprintf(stdout, "delegate %s\n", version); err != nil {
			return 1
		}
		return 0

	default:
		if _, err := fmt.Fprintf(stderr, "error: unknown command or flag %q\n\n%s", args[0], usageText); err != nil {
			return 1
		}
		return 2
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
