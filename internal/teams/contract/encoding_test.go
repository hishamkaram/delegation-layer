package contract

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func encodingTeamDocument(modelJSON []byte) []byte {
	data := []byte(`{"schema_version":1,"id":"review","name":"Review","purpose":"Review changes","roles":[{"id":"reviewer","name":"Reviewer","responsibility":"Find defects"}],"members":[{"id":"primary","name":"Primary","role_ids":["reviewer"],"execution_defaults":{"provider":"codex:exec","model":`)
	data = append(data, modelJSON...)
	return append(data, []byte(`}}]}`)...)
}

func TestTeamDecodeRejectsUnicodeRepair(t *testing.T) {
	for _, test := range []struct {
		name  string
		model []byte
	}{
		{"invalid literal UTF-8", []byte{'"', 'm', 0xff, '"'}},
		{"overlong UTF-8", []byte{'"', 'm', 0xc0, 0xaf, '"'}},
		{"raw UTF-8 surrogate", []byte{'"', 'm', 0xed, 0xa0, 0x80, '"'}},
		{"unpaired high surrogate", []byte(`"vendor/\ud800"`)},
		{"unpaired low surrogate", []byte(`"vendor/\udfff"`)},
		{"high followed by scalar", []byte(`"vendor/\ud800\u0041"`)},
		{"high followed by high", []byte(`"vendor/\ud800\udbff"`)},
		{"low followed by high", []byte(`"vendor/\udc00\ud800"`)},
		{"separated surrogates", []byte(`"vendor/\ud800x\udc00"`)},
		{"escaped backslash breaks pair", []byte(`"vendor/\ud800\\udc00"`)},
		{"valid pair then unpaired", []byte(`"vendor/\ud83d\ude00-\ud800"`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := encodingTeamDocument(test.model)
			before := bytes.Clone(data)
			team, err := Decode(data)
			if !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("Decode accepted repairable Unicode: model=%q result=%+v err=%v", test.model, team, err)
			}
			if !reflect.DeepEqual(team, Team{}) || !bytes.Equal(data, before) {
				t.Fatal("invalid encoding returned a partial Team or changed caller bytes")
			}
		})
	}
}

func TestTeamDecodePreservesValidUnicodeAndLiteralEscapes(t *testing.T) {
	for _, test := range []struct{ name, modelJSON, want string }{
		{"literal replacement rune", `"vendor/�"`, "vendor/\ufffd"},
		{"escaped replacement rune", `"vendor/\ufffd"`, "vendor/\ufffd"},
		{"paired surrogates", `"vendor/\ud83d\ude00"`, "vendor/\U0001f600"},
		{"uppercase paired surrogates", `"vendor/\uD83D\uDE00"`, "vendor/\U0001f600"},
		{"first supplementary scalar", `"vendor/\ud800\udc00"`, "vendor/\U00010000"},
		{"last Unicode scalar", `"vendor/\udbff\udfff"`, "vendor/\U0010ffff"},
		{"escaped literal backslash-u", `"vendor/\\ud800"`, `vendor/\ud800`},
		{"Unicode-escaped literal backslash-u", `"vendor/\u005cud800"`, `vendor/\ud800`},
		{"escaped quote and literal backslash-u", `"vendor/\"-\\ud800"`, `vendor/"-\ud800`},
		{"escaped backslash then valid pair", `"vendor/\\\ud83d\ude00"`, "vendor/\\\U0001f600"},
	} {
		t.Run(test.name, func(t *testing.T) {
			team, err := Decode(encodingTeamDocument([]byte(test.modelJSON)))
			if err != nil {
				t.Fatal(err)
			}
			if got := team.Members[0].ExecutionDefaults.Model; got != test.want {
				t.Fatalf("literal model changed: got %q want %q", got, test.want)
			}
			encoded, err := Encode(team)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := Decode(encoded)
			if err != nil || !reflect.DeepEqual(roundTrip, team) {
				t.Fatalf("canonical round trip changed valid Unicode: %+v %v", roundTrip, err)
			}
		})
	}
}
