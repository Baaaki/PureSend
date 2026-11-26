package update

import (
	"crypto/ed25519"

	"puresend/internal/minisign"
)

// minisignKey is a minisign public key, delegating verification to
// internal/minisign.
type minisignKey struct {
	id  [8]byte
	key ed25519.PublicKey
}

func parseMinisignKey(s string) (minisignKey, error) {
	k, err := minisign.ParseKey(s)
	if err != nil {
		return minisignKey{}, err
	}
	return minisignKey{id: k.ID, key: k.Key}, nil
}

func verifyMinisign(k minisignKey, message, sigFile []byte) error {
	return minisign.Verify(minisign.PublicKey{ID: k.id, Key: k.key}, message, sigFile)
}
