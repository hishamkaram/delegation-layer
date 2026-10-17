package app

import (
	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
)

func staticAdmissionOptions(deps Dependencies) admissioncore.StaticOptions {
	normalized := deps.normalized()
	return admissioncore.StaticOptions{Catalog: normalized.Catalog, Profiles: admissionProfiles(normalized)}
}
