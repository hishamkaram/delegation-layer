package recurrence

import "time"

// Warnings describes omission policy for an already validated definition. It
// performs no recurrence scan, reads no clock, and never predicts future laws.
func Warnings(expression, zone string) []Warning {
	if err := validateGrammar(expression); err != nil {
		return nil
	}
	location, err := namedLocation(zone)
	if err != nil {
		return nil
	}
	warnings := []Warning{{
		Code:    "calendar_omission",
		Message: "Nonexistent calendar dates are skipped; February 29 occurs only in leap years.",
	}}
	start, end := time.Unix(0, 0).In(location).ZoneBounds()
	if !start.IsZero() || !end.IsZero() {
		warnings = append(warnings, Warning{
			Code:    "timezone_omission",
			Message: "Zone rules may omit or repeat local minutes: missing minutes are skipped and repeated minutes use the first occurrence. This is not a prediction of future transitions.",
		})
	}
	return warnings
}
