package admission

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
)

type PreparedAdmission struct {
	Profile          commonprovider.PreparedProfile
	Supervisor       *pueue.Client
	RunnerExecutable string
	RunnerOwnership  string
	Capability       CapabilityEvidence
}

type PreparationOptions struct {
	Profiles         Profiles
	Supervisor       supervisorcore.AdmissionOptions
	DescribeProvider ProviderDescriptionReader
}

// ProviderDescriptionReader supplies the catalog declaration at the workflow's
// capability-observation boundary. The returned description is detached data.
type ProviderDescriptionReader func(string) (commonprovider.Description, error)

type CapabilityEvidence struct {
	Description commonprovider.Description
	Runtime     *RuntimeCapabilityEvidence
}

type RuntimeCapabilityEvidence struct {
	Version          string
	ExecutableSHA256 string
	HelpArgs         []string
	RequiredFlags    []string
}
