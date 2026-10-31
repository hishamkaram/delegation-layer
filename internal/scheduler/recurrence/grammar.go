package recurrence

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

// This fence excludes backend extensions and empty list elements. The library
// remains responsible for names, field ranges, steps and DOM/DOW semantics.
var fieldElement = regexp.MustCompile(`^(\*|([0-9]+|[A-Za-z]{3})(-([0-9]+|[A-Za-z]{3}))?)(/[0-9]+)?$`)

func validateGrammar(expression string) error {
	if len(expression) > contract.MaxOpaqueTextBytes || strings.ContainsFunc(expression, unicode.IsControl) {
		return fmt.Errorf("%w: expected at most %d bytes without control characters", ErrInvalidExpression, contract.MaxOpaqueTextBytes)
	}
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return fmt.Errorf("%w: expected five minute/hour/day/month/weekday fields", ErrInvalidExpression)
	}
	for _, field := range fields {
		for _, element := range strings.Split(field, ",") {
			if !fieldElement.MatchString(element) {
				return fmt.Errorf("%w: unsupported field element %q", ErrInvalidExpression, element)
			}
		}
	}
	return nil
}
