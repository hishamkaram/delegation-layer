package admission

// InspectionOptions is immutable launch configuration for one preflight.
type InspectionOptions struct {
	Runner             string
	DefaultRunner      string
	RunnerOwnership    string
	PreserveGeneration bool
}
