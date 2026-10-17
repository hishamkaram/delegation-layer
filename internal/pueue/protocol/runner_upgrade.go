// Package protocol contains pure Pueue protocol values and parsers.
package protocol

type RunnerCommandReplacement struct {
	NumericID              int64
	RootID                 string
	TaskID                 string
	Label                  string
	RootPath               string
	OldRunner              string
	PreviousManagedRunners []string
	NewRunner              string
	Inspection             bool
	InspectionGroup        string
}
