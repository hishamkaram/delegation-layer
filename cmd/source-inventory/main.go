// Command source-inventory prints maintained source paths for contributor tools.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/sourceinventory"
)

func run(args []string, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("source-inventory", flag.ContinueOnError)
	flags.SetOutput(stderr)
	kind := flags.String("kind", "", "source kind: go, python, javascript, or shell (required)")
	root := flags.String("root", ".", "repository root")
	allowEmpty := flags.Bool("allow-empty", false, "allow an empty Go selection for the formatter")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if flags.NArg() != 0 {
		return 2, errors.New("unexpected positional arguments")
	}
	paths, err := sourceinventory.Paths(*root, *kind, *allowEmpty)
	if err != nil {
		return 2, err
	}
	for _, path := range paths {
		record := path + "\x00"
		written, writeErr := io.WriteString(stdout, record)
		if writeErr != nil {
			return 2, fmt.Errorf("write source inventory: %w", writeErr)
		}
		if written != len(record) {
			return 2, fmt.Errorf("write source inventory: %w", io.ErrShortWrite)
		}
	}
	return 0, nil
}

func main() {
	code, err := run(os.Args[1:], os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
