package randomid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewRandomID generates a 32-character lowercase hex string from 16 cryptographically random bytes.
func NewRandomID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating random ID: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// NewTaskID generates a valid random 32-character lowercase hex task ID.
func NewTaskID() (string, error) {
	return NewRandomID()
}

// NewRootID generates a valid random 32-character lowercase hex root ID.
func NewRootID() (string, error) {
	return NewRandomID()
}
