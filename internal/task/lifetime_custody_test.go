package task

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func validCustodyPlacement() *LifetimeCustodyPlacement {
	return &LifetimeCustodyPlacement{
		Version:  LifetimeCustodyPlacementVersion,
		PoolRoot: "/var/lib/delegate/pool",
		PoolID:   strings.Repeat("ab", 32),
	}
}

func TestLifetimeCustodyPlacementValidationIsShapeOnly(t *testing.T) {
	if err := ValidateLifetimeCustodyPlacement(validCustodyPlacement()); err != nil {
		t.Fatalf("valid placement rejected: %v", err)
	}
	missing := t.TempDir() + string(os.PathSeparator) + "never-created"
	placement := validCustodyPlacement()
	placement.PoolRoot = missing
	if err := ValidateLifetimeCustodyPlacement(placement); err != nil {
		t.Fatalf("nonexistent pool root rejected: %v", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation touched the filesystem: %v", err)
	}
	if err := ValidateLifetimeCustodyPlacement(nil); err == nil {
		t.Fatal("nil placement accepted")
	}
}

func TestLifetimeCustodyPlacementValidationRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*LifetimeCustodyPlacement)
		wantErr error
	}{
		{"version zero", func(p *LifetimeCustodyPlacement) { p.Version = 0 }, ErrUnsupportedSchema},
		{"version two", func(p *LifetimeCustodyPlacement) { p.Version = 2 }, ErrUnsupportedSchema},
		{"version negative", func(p *LifetimeCustodyPlacement) { p.Version = -1 }, ErrUnsupportedSchema},
		{"empty root", func(p *LifetimeCustodyPlacement) { p.PoolRoot = "" }, ErrIdentityMismatch},
		{"blank root", func(p *LifetimeCustodyPlacement) { p.PoolRoot = "  " }, ErrIdentityMismatch},
		{"relative root", func(p *LifetimeCustodyPlacement) { p.PoolRoot = "pool/root" }, ErrIdentityMismatch},
		{"dot root", func(p *LifetimeCustodyPlacement) { p.PoolRoot = "." }, ErrIdentityMismatch},
		{"parent element", func(p *LifetimeCustodyPlacement) { p.PoolRoot = "/var/lib/../pool" }, ErrIdentityMismatch},
		{"current element", func(p *LifetimeCustodyPlacement) { p.PoolRoot = "/var/./pool" }, ErrIdentityMismatch},
		{"trailing separator", func(p *LifetimeCustodyPlacement) { p.PoolRoot = "/var/lib/pool/" }, ErrIdentityMismatch},
		{"doubled separator", func(p *LifetimeCustodyPlacement) { p.PoolRoot = "/var//pool" }, ErrIdentityMismatch},
		{"NUL in root", func(p *LifetimeCustodyPlacement) { p.PoolRoot = "/var/pool\x00x" }, ErrIdentityMismatch},
		{"empty id", func(p *LifetimeCustodyPlacement) { p.PoolID = "" }, nil},
		{"short id", func(p *LifetimeCustodyPlacement) { p.PoolID = strings.Repeat("a", 63) }, nil},
		{"long id", func(p *LifetimeCustodyPlacement) { p.PoolID = strings.Repeat("a", 65) }, nil},
		{"uppercase id", func(p *LifetimeCustodyPlacement) { p.PoolID = strings.Repeat("A", 64) }, nil},
		{"nonhex id", func(p *LifetimeCustodyPlacement) { p.PoolID = strings.Repeat("g", 64) }, nil},
		{"newline id", func(p *LifetimeCustodyPlacement) { p.PoolID = strings.Repeat("a", 64) + "\n" }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placement := validCustodyPlacement()
			tc.mutate(placement)
			err := ValidateLifetimeCustodyPlacement(placement)
			if err == nil {
				t.Fatal("invalid placement accepted")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLifetimeRegistrationValidatesPresentPlacementAfterLegacyFields(t *testing.T) {
	record := validLifetimeRegistration()
	record.CustodyPlacement = validCustodyPlacement()
	if err := ValidateLifetimeRegistrationRecord(&record); err != nil {
		t.Fatalf("registration with valid placement rejected: %v", err)
	}
	record.CustodyPlacement.PoolRoot = "relative"
	if err := ValidateLifetimeRegistrationRecord(&record); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("registration with invalid placement: %v", err)
	}

	// Legacy checks keep their precedence, so existing callers see the same
	// error for an already-invalid registration.
	record.OwnerKind = "unknown"
	if err := ValidateLifetimeRegistrationRecord(&record); !errors.Is(err, ErrInvalidEnum) {
		t.Fatalf("legacy error did not precede placement error: %v", err)
	}
	record = validLifetimeRegistration()
	record.CreatedAt = "invalid"
	record.CustodyPlacement = &LifetimeCustodyPlacement{}
	if err := ValidateLifetimeRegistrationRecord(&record); err == nil || errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("legacy timestamp error did not precede placement error: %v", err)
	}
}

func TestLifetimeRegistrationStrictJSONRefusesMalformedPlacement(t *testing.T) {
	const poolID = `"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`
	for _, tc := range []struct {
		name      string
		placement string
	}{
		{"null", `null`},
		{"not an object", `"/var/lib/pool"`},
		{"array", `[]`},
		{"empty object", `{}`},
		{"missing version", `{"pool_root":"/p","pool_id":` + poolID + `}`},
		{"missing pool root", `{"version":1,"pool_id":` + poolID + `}`},
		{"missing pool id", `{"version":1,"pool_root":"/p"}`},
		{"null version", `{"version":null,"pool_root":"/p","pool_id":` + poolID + `}`},
		{"null pool root", `{"version":1,"pool_root":null,"pool_id":` + poolID + `}`},
		{"null pool id", `{"version":1,"pool_root":"/p","pool_id":null}`},
		{"string version", `{"version":"1","pool_root":"/p","pool_id":` + poolID + `}`},
		{"fractional version", `{"version":1.5,"pool_root":"/p","pool_id":` + poolID + `}`},
		{"numeric pool root", `{"version":1,"pool_root":7,"pool_id":` + poolID + `}`},
		{"numeric pool id", `{"version":1,"pool_root":"/p","pool_id":7}`},
		{"unknown field", `{"version":1,"pool_root":"/p","pool_id":` + poolID + `,"uid":0}`},
		{"noncanonical case", `{"version":1,"Pool_Root":"/p","pool_id":` + poolID + `}`},
		{"duplicate field", `{"version":1,"pool_root":"/p","pool_root":"/q","pool_id":` + poolID + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(legacyLifetimeRegistrationJSON + `,"custody_placement":` + tc.placement + "}\n")
			var decoded LifetimeRegistrationRecord
			if err := DecodeStrict(data, &decoded); err == nil {
				t.Fatalf("malformed placement accepted: %+v", decoded.CustodyPlacement)
			}
		})
	}
	duplicate := []byte(legacyLifetimeRegistrationJSON + custodyPlacementJSON + custodyPlacementJSON + "}\n")
	var decoded LifetimeRegistrationRecord
	if err := DecodeStrict(duplicate, &decoded); err == nil {
		t.Fatal("duplicate custody_placement member accepted")
	}
}

func TestLifetimeRegistrationPlacementIsRefusedByValueAfterDecode(t *testing.T) {
	data := []byte(strings.Replace(legacyLifetimeRegistrationJSON+custodyPlacementJSON+"}\n", `"version":1`, `"version":2`, 1))
	var decoded LifetimeRegistrationRecord
	if err := DecodeStrict(data, &decoded); err != nil {
		t.Fatalf("well-shaped placement rejected by decoder: %v", err)
	}
	if err := ValidateLifetimeRegistrationRecord(&decoded); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("unsupported placement version: %v", err)
	}
}
