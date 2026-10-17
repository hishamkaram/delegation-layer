package randomid

import (
	"encoding/hex"
	"testing"
)

var (
	_ func() (string, error) = NewRandomID
	_ func() (string, error) = NewTaskID
	_ func() (string, error) = NewRootID
)

func TestIDGeneratorsPreserveShapeAndFreshness(t *testing.T) {
	generators := []struct {
		name     string
		generate func() (string, error)
	}{
		{name: "random", generate: NewRandomID},
		{name: "task", generate: NewTaskID},
		{name: "root", generate: NewRootID},
	}
	for _, generator := range generators {
		t.Run(generator.name, func(t *testing.T) {
			first, err := generator.generate()
			if err != nil {
				t.Fatal(err)
			}
			second, err := generator.generate()
			if err != nil {
				t.Fatal(err)
			}
			if first == second {
				t.Fatalf("consecutive generated IDs are equal: %q", first)
			}
			for _, id := range []string{first, second} {
				decoded, decodeErr := hex.DecodeString(id)
				if decodeErr != nil || len(decoded) != 16 || len(id) != 32 || hex.EncodeToString(decoded) != id {
					t.Fatalf("generated ID %q is not 16-byte lowercase hex", id)
				}
			}
		})
	}
}
