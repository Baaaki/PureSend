package minisign

import "testing"

func TestDefaultPublicKeyParses(t *testing.T) {
	k, err := ParseKey(DefaultPublicKey)
	if err != nil {
		t.Fatalf("DefaultPublicKey is not valid: %v", err)
	}
	if len(k.Key) == 0 {
		t.Fatal("parsed key is empty")
	}
}
