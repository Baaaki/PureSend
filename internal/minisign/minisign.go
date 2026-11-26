// Package minisign verifies Minisign Ed25519 signatures over release
// checksums and server lists without external tools or certificate chains.
package minisign

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// DefaultPublicKey is the minisign public key official PureSend releases are
// signed with (matching PUBKEY in install.sh / install.ps1 and SECURITY.md).
const DefaultPublicKey = "RWQ2F1ZFuTGorH4GqU4qC3PzJo5Evx2OKfNfJSiLbgyoEkMFDwUV8Kts"

// PublicKey is a parsed minisign Ed25519 public key and its 8-byte key ID.
type PublicKey struct {
	ID  [8]byte
	Key ed25519.PublicKey
}

// ParseKey reads a minisign public key, either the bare base64 line or the
// whole .pub file with its untrusted comment line.
func ParseKey(s string) (PublicKey, error) {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil || len(raw) != 2+8+ed25519.PublicKeySize || string(raw[:2]) != "Ed" {
		return PublicKey{}, errors.New("not a minisign public key")
	}
	var k PublicKey
	copy(k.ID[:], raw[2:10])
	k.Key = ed25519.PublicKey(raw[10:])
	return k, nil
}

// Verify checks a .minisig file over message. Both kinds of signature minisign
// makes are accepted: over the message itself ("Ed", legacy) and over its
// BLAKE2b-512 digest ("ED", default since minisign 0.8). The trusted comment
// is verified alongside the payload signature.
func Verify(k PublicKey, message, sigFile []byte) error {
	lines := strings.Split(strings.ReplaceAll(string(sigFile), "\r\n", "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "untrusted comment:") ||
		!strings.HasPrefix(lines[2], "trusted comment: ") {
		return errors.New("the signature file is malformed")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil || len(sig) != 2+8+ed25519.SignatureSize {
		return errors.New("the signature is malformed")
	}
	global, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil || len(global) != ed25519.SignatureSize {
		return errors.New("the signature is malformed")
	}

	algorithm, keyID, signature := string(sig[:2]), sig[2:10], sig[10:]
	if !bytes.Equal(keyID, k.ID[:]) {
		return fmt.Errorf("signed with a different key (%X)", keyID)
	}
	signed := message
	switch algorithm {
	case "Ed":
	case "ED":
		digest := blake2b.Sum512(message)
		signed = digest[:]
	default:
		return fmt.Errorf("unknown signature algorithm %q", algorithm)
	}
	if !ed25519.Verify(k.Key, signed, signature) {
		return errors.New("the signature does not match")
	}
	trusted := strings.TrimPrefix(lines[2], "trusted comment: ")
	if !ed25519.Verify(k.Key, append(append([]byte(nil), signature...), trusted...), global) {
		return errors.New("the trusted comment was altered")
	}
	return nil
}
