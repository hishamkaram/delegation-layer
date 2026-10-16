package lifetime

import "testing"

func TestCompareBootIDsRequiresStableComparableIdentity(t *testing.T) {
	const first = "3b76334b-df06-4d79-9863-830c44343589"
	const second = "4b76334b-df06-4d79-9863-830c44343589"
	cases := []struct {
		name, recorded, current string
		want                    BootRelation
	}{
		{"linux same", "linux:" + first, "linux:" + first, BootSame},
		{"linux different", "linux:" + first, "linux:" + second, BootDifferent},
		{"Darwin same", "darwin:boot-session:" + first, "darwin:boot-session:" + first, BootSame},
		{"Darwin different", "darwin:boot-session:" + first, "darwin:boot-session:" + second, BootDifferent},
		{"UUID case", "darwin:boot-session:3B76334B-DF06-4D79-9863-830C44343589", "darwin:boot-session:" + first, BootSame},
		{"legacy clock adjustment", "darwin:1790770768:55074", "darwin:1790771768:55074", BootUnknown},
		{"legacy to stable", "darwin:1790770768:55074", "darwin:boot-session:" + first, BootUnknown},
		{"cross OS", "linux:" + first, "darwin:boot-session:" + second, BootUnknown},
		{"missing", "", "linux:" + first, BootUnknown},
		{"unknown equal", "old-boot", "old-boot", BootUnknown},
		{"unknown different", "old-boot", "new-boot", BootUnknown},
		{"malformed Linux UUID", "linux:3b76334b-df06-4d79-9863-830c4434358z", "linux:" + first, BootUnknown},
		{"malformed Darwin UUID", "darwin:boot-session:" + first + "\n", "darwin:boot-session:" + second, BootUnknown},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := CompareBootIDs(test.recorded, test.current); got != test.want {
				t.Fatalf("CompareBootIDs(%q, %q) = %v, want %v", test.recorded, test.current, got, test.want)
			}
			if got := CompareBootIDs(test.current, test.recorded); got != test.want {
				t.Fatalf("reverse comparison = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCanonicalBootUUIDRejectsMalformedKernelValues(t *testing.T) {
	for _, value := range []string{"", "not-a-uuid", "3b76334b_df06-4d79-9863-830c44343589", "3b76334b-df06-4d79-9863-830c44343589\x00", "3b76334b-df06-4d79-9863-830c44343589 ", "3b76334b-df06-4d79-9863-830c4434358z"} {
		if _, valid := canonicalBootUUID(value); valid {
			t.Fatalf("accepted malformed boot UUID %q", value)
		}
	}
}
