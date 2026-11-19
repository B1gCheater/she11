// Package identity derives the stable machine fingerprint and client id
// WITHOUT admin privileges and WITHOUT touching the disk.
package identity

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// FingerprintHex returns hex(sha256(machineSecret)).
func FingerprintHex(secret []byte) string {
	s := sha256.Sum256(secret)
	return hex.EncodeToString(s[:])
}

// ClientID returns the 8-char human-friendly id:
// base32(sha256(secret))[:8], lowercase, no padding.
func ClientID(secret []byte) string {
	s := sha256.Sum256(secret)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(s[:]))[:8]
}
