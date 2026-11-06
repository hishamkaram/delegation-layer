// Command maintainability verifies contributor boundaries; it is not a shipped CLI.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/maintainability"
)

type commandOptions struct {
	strict        bool
	jsonOutput    bool
	goSources     bool
	sources       string
	sourceRequest bool
	root          string
}

func run(args []string, stdout, stderr io.Writer) (int, error) {
	options, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if options.goSources || options.sourceRequest {
		return printSources(options, stdout)
	}
	report, err := maintainability.Audit(options.root, options.strict, options.jsonOutput)
	if err != nil {
		return 2, err
	}
	if options.jsonOutput {
		if err = json.NewEncoder(stdout).Encode(report); err != nil {
			return 2, err
		}
	} else {
		for _, finding := range report.Findings {
			if _, err = fmt.Fprintln(stderr, maintainability.Describe(finding)); err != nil {
				return 2, err
			}
		}
		if _, err = fmt.Fprintf(stdout, "Maintainability: %d findings; %d unchanged baseline findings remain.\n", len(report.Findings), report.Deferred); err != nil {
			return 2, err
		}
	}
	if len(report.Findings) > 0 {
		return 1, nil
	}
	return 0, nil
}

func parseOptions(args []string, stderr io.Writer) (commandOptions, error) {
	var options commandOptions
	flags := flag.NewFlagSet("maintainability", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.BoolVar(&options.strict, "strict", false, "require an empty migration inventory")
	flags.BoolVar(&options.jsonOutput, "json", false, "return findings and source observations as JSON")
	flags.BoolVar(&options.goSources, "go-sources", false, "print maintained Go paths separated by NUL")
	flags.StringVar(&options.sources, "sources", "", "print maintained go, python, javascript, or shell paths separated by NUL")
	flags.StringVar(&options.root, "root", ".", "repository root")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	flags.Visit(func(option *flag.Flag) {
		if option.Name == "sources" {
			options.sourceRequest = true
		}
	})
	if flags.NArg() != 0 {
		return options, errors.New("unexpected positional arguments")
	}
	if options.goSources && options.sourceRequest {
		return options, errors.New("--sources and --go-sources are mutually exclusive")
	}
	if (options.goSources || options.sourceRequest) && (options.strict || options.jsonOutput) {
		return options, errors.New("source selection cannot be combined with --strict or --json")
	}
	return options, nil
}

func printSources(options commandOptions, stdout io.Writer) (int, error) {
	var paths []string
	var err error
	if options.goSources {
		paths, err = maintainability.GoSourcePaths(options.root)
	} else {
		paths, err = maintainability.SourcePaths(options.root, options.sources)
	}
	if err != nil {
		return 2, err
	}
	for _, path := range paths {
		if _, err = fmt.Fprintf(stdout, "%s\x00", path); err != nil {
			return 2, err
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
