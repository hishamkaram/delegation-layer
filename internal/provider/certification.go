package provider

// MatchesPrepared binds an Executed certification to the prepared runtime,
// permission profile, interpreter, and optional output-writer contract. It
// grants no execution authority and does not inspect the current host; native
// adapters resolve those facts before calling it.
func (c CertifiedProfile) MatchesPrepared(prepared PreparedProfile, osName, arch string) bool {
	policy := prepared.Effective.Policy
	return c.Status == "Executed" && c.ProviderVersion == prepared.ObservedVersion &&
		c.OS == osName && c.Arch == arch && c.Mode == prepared.Effective.Containment &&
		c.Approval == prepared.Effective.Approval && c.Predicate.Equal(prepared.Plan.Predicate) &&
		c.OutputWriterContract == prepared.Plan.OutputWriterContract && policy != nil &&
		c.RuntimeSHA256 == policy.RuntimeSHA256 && c.ProfileRevision == policy.ProfileRevision
}
