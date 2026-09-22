package provider

import "testing"

func TestValidateModelCatalogRequiresModelArray(t *testing.T) {
	catalog := ModelCatalog{
		Status:     "blocked",
		ReasonCode: "authentication_unavailable",
		Source:     "test",
	}
	if err := ValidateModelCatalog(catalog); err == nil {
		t.Fatal("nil model slice was accepted")
	}

	catalog.Models = []ModelInfo{}
	if err := ValidateModelCatalog(catalog); err != nil {
		t.Fatalf("empty model array was rejected: %v", err)
	}
}
