// Command maintainability verifies contributor boundaries; it is not a shipped CLI.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/maintainability"
)

func run() int {
	strict := flag.Bool("strict", false, "require an empty migration inventory")
	jsonOutput := flag.Bool("json", false, "return findings and source observations as JSON")
	goSources := flag.Bool("go-sources", false, "print maintained Go paths separated by NUL")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		return 2
	}
	if *goSources {
		if *strict || *jsonOutput {
			fmt.Fprintln(os.Stderr, "--go-sources cannot be combined with --strict or --json")
			return 2
		}
		return printGoSources(*root)
	}
	report, err := maintainability.Audit(*root, *strict, *jsonOutput)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if *jsonOutput {
		if err = json.NewEncoder(os.Stdout).Encode(report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	} else {
		for _, finding := range report.Findings {
			fmt.Fprintln(os.Stderr, maintainability.Describe(finding))
		}
		fmt.Printf("Maintainability: %d findings; %d unchanged baseline findings remain.\n", len(report.Findings), report.Deferred)
	}
	if len(report.Findings) > 0 {
		return 1
	}
	return 0
}

func printGoSources(root string) int {
	paths, err := maintainability.GoSourcePaths(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, path := range paths {
		if _, err = fmt.Fprintf(os.Stdout, "%s\x00", path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	return 0
}

func main() { os.Exit(run()) }
