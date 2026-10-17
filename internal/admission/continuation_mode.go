package admission

import (
	"fmt"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func ContinuationProviderMode(catalog commonprovider.Catalog, provider string) (commonprovider.ContinuationMode, error) {
	registration, err := catalog.Lookup(provider)
	if err != nil {
		return "", WithStage("resolve-continuation-provider", err)
	}
	if registration.Description.Continuation != commonprovider.ContinuationNative {
		return "", WithStage("check-continuation-capability", fmt.Errorf("native continuation unsupported for %s", provider))
	}
	return registration.Description.Continuation, nil
}
