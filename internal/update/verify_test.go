package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// Signatures made by an independent implementation (aead.dev/minisign
// v0.3.0), so the verifier is checked against minisign itself and not
// only against its own idea of the format.
const (
	vectorKey     = "RWTBZjpvyDHND+qd3Or37BvrSo1yi70CBj/ykrzX7p6WlXvZce6+BuKa"
	vectorMessage = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  puresend_1.1.0_linux_x86_64.tar.gz\n"
	vectorLegacy  = "untrusted comment: signature from minisign secret key\nRWTBZjpvyDHND0KUJJg0Xfgw0oLrWYCThU5lg5ypd6v0lC+8QiBy3O7HmuF8f7CoHv8nF079meIoeGYd+r27OwFmjF0jt13kPgE=\ntrusted comment: timestamp:1790000000\tfile:checksums.txt\nFurY6W7C9YudcLuBRxQF2b8q+NvU4n+qMUV87YLA83x1Zyy5hzmzMB5GjImqnaLVIgN7O6i4lSaY1mNoCFfgAw==\n"
	vectorHashed  = "untrusted comment: signature from minisign secret key\nRUTBZjpvyDHND8gUVaklyPQ/Fp/Bu29KUNz0wILtJf8JEpo7LVsTDMtvEu9uYOTMp1PLHys0LLWAOuT1MHHBfjixZ8AE/0pBWws=\ntrusted comment: timestamp:1790000000\tfile:checksums.txt\tprehashed\nK6frVmr4zVyqAj7X+iW1/3OuDkoG61eD/3lCIT5eUAqdNEbo+zoh8vLXEnzaHf3UArgpmf4yZ7fwAwQBtsbhAg==\n"
)

func TestMinisignVectors(t *testing.T) {
	key, err := parseMinisignKey("untrusted comment: minisign public key\n" + vectorKey + "\n")
	if err != nil {
		t.Fatal(err)
	}
	for name, sig := range map[string]string{"legacy (Ed)": vectorLegacy, "prehashed (ED)": vectorHashed} {
		t.Run(name, func(t *testing.T) {
			if err := verifyMinisign(key, []byte(vectorMessage), []byte(sig)); err != nil {
				t.Fatalf("a genuine signature was refused: %v", err)
			}
			if err := verifyMinisign(key, []byte(strings.Replace(vectorMessage, "e3b0", "e3b1", 1)), []byte(sig)); err == nil {
				t.Error("a changed message was accepted")
			}
			forged := strings.Replace(sig, "timestamp:1790000000", "timestamp:1790000001", 1)
			if err := verifyMinisign(key, []byte(vectorMessage), []byte(forged)); err == nil {
				t.Error("a changed trusted comment was accepted")
			}
		})
	}

	other := newTestKey(t)
	if err := verifyMinisign(other.key, []byte(vectorMessage), []byte(vectorHashed)); err == nil {
		t.Error("a signature by another key was accepted")
	}
	for _, bad := range []string{"", "not base64", "RWQ=", vectorKey[:len(vectorKey)-4]} {
		if _, err := parseMinisignKey(bad); err == nil {
			t.Errorf("parseMinisignKey(%q) accepted a bad key", bad)
		}
	}
}

// testKey is a minisign key pair made up for a test.
type testKey struct {
	key  minisignKey
	priv ed25519.PrivateKey
	text string // the public key as the ldflags carry it
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var k testKey
	_, _ = rand.Read(k.key.id[:])
	k.key.key, k.priv = pub, priv
	k.text = base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), k.key.id[:]...), pub...))
	return k
}

// sign makes a prehashed minisign signature file, the kind `minisign -S`
// writes by default.
func (k testKey) sign(message []byte) []byte {
	digest := blake2b.Sum512(message)
	sig := ed25519.Sign(k.priv, digest[:])
	trusted := "timestamp:1790000000\tfile:checksums.txt"
	global := ed25519.Sign(k.priv, append(append([]byte(nil), sig...), trusted...))
	return []byte("untrusted comment: signature from minisign secret key\n" +
		base64.StdEncoding.EncodeToString(append(append([]byte("ED"), k.key.id[:]...), sig...)) + "\n" +
		"trusted comment: " + trusted + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n")
}

// release serves a fake release's assets and lists them the way the
// GitHub API does.
func release(t *testing.T, files map[string][]byte) []Asset {
	t.Helper()
	saved := allowInsecureDownload
	allowInsecureDownload = true
	t.Cleanup(func() { allowInsecureDownload = saved })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	var assets []Asset
	for name := range files {
		assets = append(assets, Asset{Name: name, BrowserDownloadURL: srv.URL + "/" + name})
	}
	return assets
}

// TestUpdateIsVerified: an update is installed only once the archive
// matches checksums.txt, and — in a build that carries a key — only once
// checksums.txt carries a signature by that key. Anything less is refused,
// not installed unchecked.
func TestUpdateIsVerified(t *testing.T) {
	const name = "puresend_1.1.0_linux_x86_64.tar.gz"
	archive := []byte("the real archive")
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n" +
		strings.Repeat("0", 64) + "  puresend_1.1.0_windows_x86_64.zip\n")
	key, stranger := newTestKey(t), newTestKey(t)

	for _, tc := range []struct {
		name  string
		files map[string][]byte
		key   string
		ok    bool
	}{
		{"checksums match, no key built in", map[string][]byte{name: archive, "checksums.txt": sums}, "", true},
		{"no checksums.txt", map[string][]byte{name: archive}, "", false},
		{"archive does not match", map[string][]byte{name: []byte("a swapped archive"), "checksums.txt": sums}, "", false},
		{"archive not listed", map[string][]byte{name: archive, "checksums.txt": sums[len(sums)/2:]}, "", false},
		{"signed by the built-in key", map[string][]byte{name: archive, "checksums.txt": sums, "checksums.txt.minisig": key.sign(sums)}, key.text, true},
		{"key built in, no signature", map[string][]byte{name: archive, "checksums.txt": sums}, key.text, false},
		{"signed by another key", map[string][]byte{name: archive, "checksums.txt": sums, "checksums.txt.minisig": stranger.sign(sums)}, key.text, false},
		{"signature over other checksums", map[string][]byte{name: archive, "checksums.txt": sums, "checksums.txt.minisig": key.sign([]byte("other"))}, key.text, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := publicKey
			publicKey = tc.key
			t.Cleanup(func() { publicKey = saved })

			assets := release(t, tc.files)
			got, err := fetchVerified(context.Background(), assets, *assetNamed(assets, name), "test")
			if tc.ok && (err != nil || string(got) != string(archive)) {
				t.Fatalf("a good release was refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("an unverified release was accepted")
			}
		})
	}
}
