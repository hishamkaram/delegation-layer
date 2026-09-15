//go:build ignore

// Generate the acceptance parser's Unicode SimpleFold translation table with
// the repository's pinned Go toolchain. Run `go generate ./internal/testutil/unicodefold`
// after an intentional Go upgrade, then run the verification test.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"unicode"
)

func main() {
	table := make(map[string]int)
	for value := rune(0); value <= unicode.MaxRune; value++ {
		minimum := minimumSimpleFold(value)
		if minimum != value {
			table[strconv.Itoa(int(value))] = int(minimum)
		}
	}
	data, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		panic(fmt.Errorf("marshal Unicode SimpleFold table: %w", err))
	}
	data = append(data, '\n')
	path := filepath.Join("..", "..", "..", "scripts", "_data", "unicode-simple-fold.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(fmt.Errorf("write Unicode SimpleFold table: %w", err))
	}
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
