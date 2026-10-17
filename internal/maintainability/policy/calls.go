package policy

import "strings"

// LargePythonPositionalCall rejects long unnamed argument transport. Named
// arguments preserve the existing API and make each supplied value explicit.
const LargePythonPositionalCall = 8

func checkPositionalCalls(source Source) []Finding {
	if !strings.HasPrefix(source.Module, "py:") || source.Kind == "generated" || source.Kind == "asset" {
		return nil
	}
	var findings []Finding
	for _, call := range source.Calls {
		if call.PositionalArguments >= LargePythonPositionalCall {
			findings = append(findings, Finding{
				Path: source.Path, Line: call.Line, Kind: "positional-call", Symbol: call.Name,
				Detail: "large Python call requires named arguments", Actual: call.PositionalArguments,
				Limit: LargePythonPositionalCall - 1,
			})
		}
	}
	return findings
}
