package app

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func admissionPreparationOptions(deps Dependencies) admissioncore.PreparationOptions {
	return admissioncore.PreparationOptions{Profiles: admissionProfiles(deps), Supervisor: supervisorAdmissionOptions(deps, deps.SupervisorOptions), DescribeProvider: func(id string) (commonprovider.Description, error) {
		registration, err := deps.normalized().Catalog.Lookup(id)
		return registration.Description, err
	}}
}
