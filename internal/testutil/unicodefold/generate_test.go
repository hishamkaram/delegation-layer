package unicodefold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"
	"unicode"
)

func TestCommittedTableMatchesGoSimpleFold(t *testing.T) {
	path := committedTablePath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed table: %v", err)
	}
	var got map[string]int
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode committed table: %v", err)
	}
	want := make(map[string]int)
	for value := rune(0); value <= unicode.MaxRune; value++ {
		minimum := minimumSimpleFold(value)
		if minimum != value {
			want[strconv.Itoa(int(value))] = int(minimum)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("committed table differs from Go unicode.SimpleFold: got %d entries, want %d", len(got), len(want))
	}
}

func TestCommittedTableHasRepresentativeSimpleFoldAliases(t *testing.T) {
	data, err := os.ReadFile(committedTablePath(t))
	if err != nil {
		t.Fatalf("read committed table: %v", err)
	}
	var table map[string]int
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatalf("decode committed table: %v", err)
	}
	for _, pair := range []struct {
		name string
		from rune
		want rune
	}{
		{name: "ASCII lowercase", from: 's', want: 'S'},
		{name: "Kelvin sign", from: '\u212A', want: 'K'},
		{name: "Greek final sigma", from: '\u03C2', want: '\u03A3'},
	} {
		t.Run(pair.name, func(t *testing.T) {
			if got := table[strconv.Itoa(int(pair.from))]; got != int(pair.want) {
				t.Fatalf("table[%U]=%U want %U", pair.from, got, pair.want)
			}
		})
	}
}

func committedTablePath(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locating Unicode SimpleFold test source")
	}
	return filepath.Join(filepath.Dir(source), "..", "..", "..", "scripts", "_data", "unicode-simple-fold.json")
}

func minimumSimpleFold(value rune) rune {
	minimum := value
	for next := unicode.SimpleFold(value); next != value; next = unicode.SimpleFold(next) {
		if next < minimum {
			minimum = next
		}
	}
	return minimum
}
