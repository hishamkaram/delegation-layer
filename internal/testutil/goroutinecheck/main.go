// Package goroutinecheck owns post-suite goroutine leak verification for test runners.
package goroutinecheck

import (
	"fmt"
	"io"
	"os"

	"go.uber.org/goleak"
)

// Run checks for leaks after all tests and their cleanup finish, preserving a
// prior test failure. Callers dispatch subprocess helper modes before Run.
func Run(tests interface{ Run() int }) int {
	return run(tests.Run, func() error { return goleak.Find() }, os.Stderr)
}

func run(runTests func() int, findLeaks func() error, output io.Writer) int {
	code := runTests()
	if err := findLeaks(); err != nil {
		if _, reportErr := fmt.Fprintf(output, "goroutinecheck: %v\n", err); reportErr != nil {
			// A failed diagnostic writer must still produce a failing exit status.
			return failureCode(code)
		}
		return failureCode(code)
	}
	return code
}

func failureCode(code int) int {
	if code == 0 {
		return 1
	}
	return code
}
