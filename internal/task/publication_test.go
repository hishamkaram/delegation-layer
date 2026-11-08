package task

import "testing"

func TestPublicationTerminal(t *testing.T) {
	for _, test := range []struct {
		publication Publication
		terminal    bool
	}{
		{PublicationUnknown, false},
		{PublicationPending, false},
		{PublicationCommitted, true},
		{PublicationRejected, true},
		{Publication(255), false},
	} {
		if got := test.publication.Terminal(); got != test.terminal {
			t.Errorf("publication %d terminal = %t, want %t", test.publication, got, test.terminal)
		}
	}
}
