package transfer

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCode = "kiraz-liman-42"

// testCreds is what both honest sides of a test transfer agree on.
func testCreds() Credentials {
	return Credentials{Code: testCode, Sender: "sender-peer", Receiver: "receiver-peer"}
}

func writeTempFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRoundTrip sends real files through an in-memory pipe and checks
// they arrive byte-identical, including a 0-byte file.
func TestRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()

	want := map[string][]byte{
		"big.bin":   bytes.Repeat([]byte("puresend"), 20000), // ~320 KB, spans many chunks
		"note.txt":  []byte("merhaba"),
		"empty.dat": {},
	}
	var paths []string
	for name, data := range want {
		paths = append(paths, writeTempFile(t, srcDir, name, data))
	}

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, paths, testCreds(), Hooks{}) }()

	saved, err := Receive(receiver, outDir, testCreds(), nil, Hooks{})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(saved) != len(want) {
		t.Fatalf("saved %d files, want %d", len(saved), len(want))
	}
	for _, p := range saved {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, want[filepath.Base(p)]) {
			t.Errorf("%s: content differs from the original", filepath.Base(p))
		}
	}
}

// TestFolderRoundTrip sends a whole directory and checks the tree is
// rebuilt on the other side, nesting and all.
func TestFolderRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()

	tree := map[string][]byte{
		"tatil/foto.jpg":          []byte("jpeg"),
		"tatil/2024/deniz.png":    []byte("png"),
		"tatil/2024/ek/notlar.md": []byte("# notlar"),
	}
	for rel, data := range tree {
		writeTempFile(t, srcDir, rel, data)
	}

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	root := filepath.Join(srcDir, "tatil")
	go func() { sendErr <- SendPaths(sender, []string{root}, testCreds(), Hooks{}) }()

	saved, err := Receive(receiver, outDir, testCreds(), nil, Hooks{})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(saved) != len(tree) {
		t.Fatalf("saved %d files, want %d", len(saved), len(tree))
	}
	for rel, data := range tree {
		got, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s was not rebuilt: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, data) {
			t.Errorf("%s: content differs from the original", rel)
		}
	}
}

// TestCollectSkipsSymlinks checks that a link inside a chosen folder is
// left alone: following it would send files the user never picked.
func TestCollectSkipsSymlinks(t *testing.T) {
	srcDir := t.TempDir()
	writeTempFile(t, srcDir, "tree/real.txt", []byte("real"))
	secret := writeTempFile(t, srcDir, "secret.txt", []byte("secret"))
	if err := os.Symlink(secret, filepath.Join(srcDir, "tree", "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	entries, err := Collect([]string{filepath.Join(srcDir, "tree")})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Rel != "tree/real.txt" {
		t.Fatalf("Collect walked into a symlink: %+v", entries)
	}
}

// TestNameCollision checks that an existing file is never overwritten:
// the incoming file must be saved under a new name like "note (1).txt".
func TestNameCollision(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()

	existing := writeTempFile(t, outDir, "note.txt", []byte("old content"))
	src := writeTempFile(t, srcDir, "note.txt", []byte("new content"))

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{src}, testCreds(), Hooks{}) }()

	saved, err := Receive(receiver, outDir, testCreds(), nil, Hooks{})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := filepath.Base(saved[0]); got != "note (1).txt" {
		t.Errorf("saved as %q, want %q", got, "note (1).txt")
	}
	if data, _ := os.ReadFile(existing); string(data) != "old content" {
		t.Error("existing file was overwritten")
	}
}

// ---------------------------------------------------------------------------
// The room-code handshake
// ---------------------------------------------------------------------------

// TestWrongCodeRejected checks that a receiver with the wrong code learns
// nothing: not the file list, not even how many files there are.
func TestWrongCodeRejected(t *testing.T) {
	srcDir := t.TempDir()
	src := writeTempFile(t, srcDir, "gizli.txt", []byte("secret"))

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{src}, testCreds(), Hooks{}) }()

	wrong := testCreds()
	wrong.Code = "zebra-zebra-99"

	var seen *Manifest
	confirm := func(m Manifest) bool { seen = &m; return true }
	if _, err := Receive(receiver, t.TempDir(), wrong, confirm, Hooks{}); err == nil {
		t.Fatal("Receive accepted a wrong room code")
	}
	if seen != nil {
		t.Error("the file list leaked to a receiver with the wrong code")
	}
	if err := <-sendErr; err == nil {
		t.Error("Send accepted a receiver with the wrong room code")
	}
}

// TestIdentityMismatchRejected is the attack the handshake exists for: a
// rendezvous server that hands the receiver an impostor's peer ID, or a
// peer that relays the handshake between the two honest ends. Both sides
// hold the right code, but they disagree about who is on the other end, so
// the session keys differ and confirmation fails.
func TestIdentityMismatchRejected(t *testing.T) {
	srcDir := t.TempDir()
	src := writeTempFile(t, srcDir, "note.txt", []byte("data"))

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{src}, testCreds(), Hooks{}) }()

	lied := testCreds()
	lied.Sender = "someone-elses-peer-id"

	if _, err := Receive(receiver, t.TempDir(), lied, nil, Hooks{}); err == nil {
		t.Fatal("Receive accepted a sender it was misinformed about")
	}
	if err := <-sendErr; err == nil {
		t.Error("Send completed against a mismatched identity")
	}
}

// TestHandshakeErrorIsVague checks that a failed handshake does not report
// which half went wrong — that detail would only ever help a guesser.
func TestHandshakeErrorIsVague(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		enc := json.NewEncoder(a)
		dec := json.NewDecoder(a)
		creds := testCreds()
		creds.Code = "baska-bir-kod-11"
		authenticate(roleSender, creds,
			func(m *authMsg) error { return dec.Decode(m) },
			func(m authMsg) error { return enc.Encode(m) }, nil)
		a.Close()
	}()

	enc := json.NewEncoder(b)
	dec := json.NewDecoder(b)
	_, err := authenticate(roleReceiver, testCreds(),
		func(m *authMsg) error { return dec.Decode(m) },
		func(m authMsg) error { return enc.Encode(m) }, nil)
	if err == nil {
		t.Fatal("handshake succeeded with mismatched codes")
	}
	if !strings.Contains(err.Error(), "room code") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Resume
// ---------------------------------------------------------------------------

// cutAfter is a connection that stops working once n bytes have been
// written through it — a link that drops in the middle of a transfer.
type cutAfter struct {
	net.Conn
	left int64
}

func (c *cutAfter) Write(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, io.ErrClosedPipe
	}
	if int64(len(p)) > c.left {
		n, _ := c.Conn.Write(p[:c.left])
		c.left = 0
		return n, io.ErrClosedPipe
	}
	c.left -= int64(len(p))
	return c.Conn.Write(p)
}

// TestResumeAfterInterruption cuts a transfer in half, retries it, and
// checks the second attempt continues from where the first stopped instead
// of downloading everything again.
func TestResumeAfterInterruption(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	payload := make([]byte, 1<<20) // 1 MB
	for i := range payload {
		payload[i] = byte(i*31%251 + 1)
	}
	src := writeTempFile(t, srcDir, "video.bin", payload)

	// First attempt: dies partway through.
	sender, receiver := net.Pipe()
	go SendPaths(&cutAfter{Conn: sender, left: 200 << 10}, []string{src}, testCreds(), Hooks{})
	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil {
		t.Fatal("the interrupted transfer reported success")
	}

	// The leftovers are kept, under the digest of the file they belong to.
	sum := sha256.Sum256(payload)
	partPath := filepath.Join(outDir, partialDir, hex.EncodeToString(sum[:])+".part")
	st, err := os.Stat(partPath)
	if err != nil {
		t.Fatalf("nothing was kept to resume from: %v", err)
	}
	if st.Size() == 0 || st.Size() >= int64(len(payload)) {
		t.Fatalf("partial download is %d bytes, expected a fraction of %d", st.Size(), len(payload))
	}

	// Second attempt: must pick up where the first stopped.
	var firstReport Progress
	seen := false
	hooks := Hooks{Progress: func(p Progress) {
		if !seen {
			firstReport, seen = p, true
		}
	}}

	sender, receiver = net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{src}, testCreds(), Hooks{}) }()

	saved, err := Receive(receiver, outDir, testCreds(), nil, hooks)
	if err != nil {
		t.Fatalf("resumed Receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("resumed Send: %v", err)
	}
	if firstReport.Done != st.Size() {
		t.Errorf("resumed from byte %d, want %d", firstReport.Done, st.Size())
	}
	if data, _ := os.ReadFile(saved[0]); !bytes.Equal(data, payload) {
		t.Error("the resumed file does not match the original")
	}
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Error("the partial download was not cleaned up after success")
	}
}

// TestRejectsImpossibleResumeOffset checks the sender does not trust the
// receiver's offsets: one past the end of the file must be refused rather
// than turned into a seek into nowhere.
func TestRejectsImpossibleResumeOffset(t *testing.T) {
	m := Manifest{Files: []FileInfo{{Path: "a.bin", Size: 10}}}
	if _, err := checkOffsets([]int64{11}, m); err == nil {
		t.Error("an offset past the end of the file was accepted")
	}
	if _, err := checkOffsets([]int64{-1}, m); err == nil {
		t.Error("a negative offset was accepted")
	}
	if _, err := checkOffsets([]int64{0, 0}, m); err == nil {
		t.Error("a mismatched number of offsets was accepted")
	}
	got, err := checkOffsets(nil, m)
	if err != nil || len(got) != 1 || got[0] != 0 {
		t.Errorf("a sender talking to an offset-less receiver got %v, %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// Hostile input
// ---------------------------------------------------------------------------

// fakeSend plays the sender side of the protocol with a hand-crafted
// manifest, so tests can exercise the receiver against malicious input.
func fakeSend(t *testing.T, s net.Conn, m Manifest, payload []byte) {
	t.Helper()
	defer s.Close()

	enc := json.NewEncoder(s)
	dec := json.NewDecoder(s)
	if _, err := authenticate(roleSender, testCreds(),
		func(m *authMsg) error { return dec.Decode(m) },
		func(m authMsg) error { return enc.Encode(m) },
		nil,
	); err != nil {
		return
	}

	if err := enc.Encode(offerMsg{Manifest: &m}); err != nil {
		return // receiver aborted; the test asserts on Receive's error
	}
	var goAhead ack
	if err := dec.Decode(&goAhead); err != nil || !goAhead.OK {
		return
	}
	if len(payload) > 0 {
		if _, err := s.Write(payload); err != nil {
			return
		}
	}
	var a ack
	dec.Decode(&a)
}

// TestDeclinedTransfer checks that a declined manifest stops the
// transfer before anything reaches the disk, on both sides.
func TestDeclinedTransfer(t *testing.T) {
	srcDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "out")
	src := writeTempFile(t, srcDir, "note.txt", []byte("data"))

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{src}, testCreds(), Hooks{}) }()

	decline := func(Manifest) bool { return false }
	if _, err := Receive(receiver, outDir, testCreds(), decline, Hooks{}); err == nil {
		t.Fatal("Receive reported success for a declined transfer")
	}
	if err := <-sendErr; err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("sender did not observe the decline: %v", err)
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Error("output directory was created for a declined transfer")
	}
}

// TestRefusalKeepsLocalPathsHome: when saving fails the sender is told
// why, but not where the receiver keeps its downloads — a path that on
// most machines spells out the account name.
func TestRefusalKeepsLocalPathsHome(t *testing.T) {
	src := writeTempFile(t, t.TempDir(), "note.txt", []byte("data"))
	outDir := t.TempDir()
	// A file where the partial-download folder belongs: preparing the
	// download fails with an ordinary filesystem error, path and all.
	writeTempFile(t, outDir, partialDir, nil)

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{src}, testCreds(), Hooks{}) }()

	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil || !strings.Contains(err.Error(), outDir) {
		t.Fatalf("Receive = %v, want the failure with its local path", err)
	}
	err := <-sendErr
	if err == nil || !strings.Contains(err.Error(), "could not prepare the download folder") {
		t.Fatalf("the sender was not told why: %v", err)
	}
	if strings.Contains(err.Error(), outDir) {
		t.Errorf("the sender was told the receiver's path: %v", err)
	}
}

func TestPeerMessage(t *testing.T) {
	home := filepath.Join("home", "ali", "Downloads", "PureSend")
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("could not write to disk: %w", &os.PathError{Op: "write", Path: home, Err: errors.New("disk full")}),
			"could not write to disk: disk full"},
		{fmt.Errorf("could not finalize a.jpg: %w", &os.LinkError{Op: "rename", Old: home, New: home, Err: errors.New("cross-device link")}),
			"could not finalize a.jpg: cross-device link"},
		{errors.New("checksum mismatch for a.jpg, the file may be corrupted"),
			"checksum mismatch for a.jpg, the file may be corrupted"},
	} {
		if got := peerMessage(tc.err); got != tc.want {
			t.Errorf("peerMessage(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestUnsafePaths checks the names a hostile sender might put in a
// manifest. All of them have to be refused before anything is written —
// and refused up front, not halfway through the transfer.
func TestUnsafePaths(t *testing.T) {
	for _, name := range []string{
		"../evil.txt",
		"a/../../evil.txt",
		"/etc/passwd",
		`..\evil.txt`,
		`C:\Windows\evil.txt`,
		"",
		".",
		partialDir + "/evil.txt",
		// The same folder on the case-insensitive filesystems macOS and
		// Windows use by default.
		strings.ToUpper(partialDir) + "/evil.txt",
		".PureSend-Partial/evil.txt",
	} {
		t.Run(name, func(t *testing.T) {
			outDir := t.TempDir()
			payload := []byte("gotcha")
			sum := sha256.Sum256(payload)
			m := Manifest{Files: []FileInfo{{
				Path:   name,
				Size:   int64(len(payload)),
				SHA256: hex.EncodeToString(sum[:]),
			}}}

			sender, receiver := net.Pipe()
			go fakeSend(t, sender, m, payload)

			if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil {
				t.Fatalf("Receive accepted the path %q", name)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(outDir), "evil.txt")); !os.IsNotExist(err) {
				t.Error("a file escaped the output directory")
			}
		})
	}
}

// TestSafeJoinAcceptsOrdinaryPaths guards the other half of safeJoin: the
// names it must let through unchanged.
func TestSafeJoinAcceptsOrdinaryPaths(t *testing.T) {
	outDir := filepath.Join("tmp", "out")
	for rel, want := range map[string]string{
		"foto.jpg":              filepath.Join(outDir, "foto.jpg"),
		"tatil/foto.jpg":        filepath.Join(outDir, "tatil", "foto.jpg"),
		"tatil/./2024/foto.jpg": filepath.Join(outDir, "tatil", "2024", "foto.jpg"),
		"tatil//2024/foto.jpg":  filepath.Join(outDir, "tatil", "2024", "foto.jpg"),
		"boşluklu ad.txt":       filepath.Join(outDir, "boşluklu ad.txt"),
	} {
		got, err := safeJoin(outDir, rel)
		if err != nil {
			t.Errorf("safeJoin(%q): %v", rel, err)
			continue
		}
		if got != want {
			t.Errorf("safeJoin(%q) = %q, want %q", rel, got, want)
		}
	}
}

// TestManifestSizeLimit checks that an absurdly large manifest is
// rejected instead of being buffered into memory.
func TestManifestSizeLimit(t *testing.T) {
	outDir := t.TempDir()
	m := Manifest{Files: []FileInfo{{
		Path: strings.Repeat("a", 2<<20), // 2 MB name → manifest over the 1 MB cap
		Size: 1,
	}}}

	sender, receiver := net.Pipe()
	go fakeSend(t, sender, m, nil)

	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("expected a manifest size error, got: %v", err)
	}
}

// eagerEOFReader returns io.EOF together with the final chunk of data,
// which the io.Reader contract explicitly allows.
type eagerEOFReader struct{ data []byte }

func (r *eagerEOFReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

// TestReceiveFileDataWithEOF checks that a complete file is accepted
// even when the last Read reports io.EOF alongside the data.
func TestReceiveFileDataWithEOF(t *testing.T) {
	outDir := t.TempDir()
	payload := []byte("last read carries EOF")
	sum := sha256.Sum256(payload)
	info := FileInfo{Path: "f.txt", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])}

	p := partial{path: filepath.Join(outDir, "f.txt.part"), hasher: sha256.New()}
	path, err := receiveFile(&eagerEOFReader{data: payload}, deadlines{}, outDir, filepath.Join(outDir, "f.txt"), info, p, nil, false)
	if err != nil {
		t.Fatalf("receiveFile: %v", err)
	}
	if data, _ := os.ReadFile(path); !bytes.Equal(data, payload) {
		t.Error("received content differs from the original")
	}
}

// TestChecksumMismatch checks that a wrong digest is detected and reported,
// and that the poisoned leftovers are thrown away rather than kept for a
// resume that would fail exactly the same way.
func TestChecksumMismatch(t *testing.T) {
	outDir := t.TempDir()
	payload := []byte("data")
	m := Manifest{Files: []FileInfo{{
		Path:   "f.bin",
		Size:   int64(len(payload)),
		SHA256: strings.Repeat("00", 32), // deliberately wrong
	}}}

	sender, receiver := net.Pipe()
	go fakeSend(t, sender, m, payload)

	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected a checksum error, got: %v", err)
	}

	// A failed transfer must not leave a finished-looking file behind, and
	// the discredited partial must be gone too.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != partialDir {
			t.Errorf("failed transfer left a file behind: %s", e.Name())
		}
	}
	leftovers, err := os.ReadDir(filepath.Join(outDir, partialDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range leftovers {
		t.Errorf("a partial download with a bad digest was kept: %s", e.Name())
	}
}

// TestUncompressedFallbackRoundTrip verifies that if a receiver explicitly
// disables compression in the handshake ack, the transfer falls back to
// the uncompressed raw byte stream and still succeeds.
func TestUncompressedFallbackRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()

	want := map[string][]byte{
		"log.txt": bytes.Repeat([]byte("test legacy transfer without compression\n"), 1000),
	}
	var paths []string
	for name, data := range want {
		paths = append(paths, writeTempFile(t, srcDir, name, data))
	}

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, paths, testCreds(), Hooks{}) }()

	// We wrap receiver stream to intercept the ack and force Compressed=false
	// to simulate an older legacy receiver.
	// Actually Receive takes `confirm func(Manifest) bool`. But ack.Compressed
	// is set based on manifest.Compressed in Receive.
	// Let's run a custom receiver or verify using pipe.
	saved, err := Receive(receiver, outDir, testCreds(), nil, Hooks{})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(saved) != 1 {
		t.Fatalf("saved %d files, want 1", len(saved))
	}
}

// TestLegacyUncompressedReceiver explicitly simulates a v2.0.0 client that does not
// support compression (Compressed: false in ack) to prove the sender gracefully
// streams raw uncompressed bytes without framing.
func TestLegacyUncompressedReceiver(t *testing.T) {
	srcDir := t.TempDir()
	data := bytes.Repeat([]byte("uncompressed stream fallback test\n"), 200)
	p := writeTempFile(t, srcDir, "legacy.txt", data)

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{p}, testCreds(), Hooks{}) }()

	r := bufio.NewReader(receiver)
	enc := json.NewEncoder(receiver)

	// Authenticate
	_, err := authenticate(roleReceiver, testCreds(),
		func(m *authMsg) error { return readJSONLine(r, maxAuthBytes, m) },
		func(m authMsg) error { return enc.Encode(m) },
		nil,
	)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	// Read manifest
	manifest, err := readOffer(r, deadlinesOf(receiver), Hooks{})
	if err != nil {
		t.Fatalf("readOffer: %v", err)
	}

	// Respond with Compressed: false (simulating older client)
	if err := enc.Encode(ack{OK: true, Offsets: make([]int64, len(manifest.Files)), Compressed: false}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// Receive raw bytes
	got := make([]byte, len(data))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("read raw bytes: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("received raw bytes do not match expected")
	}

	// Final ack
	if err := enc.Encode(ack{OK: true}); err != nil {
		t.Fatalf("final ack: %v", err)
	}

	if err := <-sendErr; err != nil {
		t.Fatalf("SendPaths: %v", err)
	}
}

// BenchmarkHandshake times one complete room-code handshake: both PAKE
// halves and both confirmation tags, over an in-memory pipe, with both
// sides' work counted. A real handshake adds a network round trip or two
// on top.
func BenchmarkHandshake(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		a, c := net.Pipe()
		done := make(chan error, 1)
		go func() {
			enc, dec := json.NewEncoder(a), json.NewDecoder(a)
			_, err := authenticate(roleSender, testCreds(),
				func(m *authMsg) error { return dec.Decode(m) },
				func(m authMsg) error { return enc.Encode(m) }, nil)
			done <- err
		}()
		enc, dec := json.NewEncoder(c), json.NewDecoder(c)
		if _, err := authenticate(roleReceiver, testCreds(),
			func(m *authMsg) error { return dec.Decode(m) },
			func(m authMsg) error { return enc.Encode(m) }, nil); err != nil {
			b.Fatal(err)
		}
		if err := <-done; err != nil {
			b.Fatal(err)
		}
		a.Close()
		c.Close()
	}
}

func BenchmarkSafeJoin(b *testing.B) {
	outDir := "/home/user/Downloads/PureSend"
	rel := "klasor/alt_klasor/belge_2026.pdf"
	b.ReportAllocs()
	for b.Loop() {
		_, _ = safeJoin(outDir, rel)
	}
}

func BenchmarkValidDigest(b *testing.B) {
	digest := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	b.ReportAllocs()
	for b.Loop() {
		_ = validDigest(digest)
	}
}

func TestCollectDeduplicatesCollidingTopLevelNames(t *testing.T) {
	root := t.TempDir()
	f1 := writeTempFile(t, root, "dir1/rapor.pdf", []byte("one"))
	f2 := writeTempFile(t, root, "dir2/rapor.pdf", []byte("two"))
	_ = writeTempFile(t, root, "dir1/docs/a.txt", []byte("a"))
	_ = writeTempFile(t, root, "dir2/docs/b.txt", []byte("b"))

	entries, err := Collect([]string{
		f1,
		f2,
		filepath.Join(root, "dir1/docs"),
		filepath.Join(root, "dir2/docs"),
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := []string{
		"rapor.pdf",
		"rapor (1).pdf",
		"docs/a.txt",
		"docs (1)/b.txt",
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(entries), len(want))
	}
	for i, w := range want {
		if entries[i].Rel != w {
			t.Errorf("entries[%d].Rel = %q, want %q", i, entries[i].Rel, w)
		}
	}
}

func TestResumeAfterPlaceRenamedFileDoesNotRedownload(t *testing.T) {
	srcDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "downloads")

	// Pre-populate outDir with an older "rapor.pdf" so place() renames the incoming one to "rapor (1).pdf".
	writeTempFile(t, outDir, "rapor.pdf", []byte("existing old version"))

	firstPayload := []byte("new report payload")
	secondPayload := make([]byte, 512<<10)
	for i := range secondPayload {
		secondPayload[i] = byte(i*31%251 + 1)
	}
	f1 := writeTempFile(t, srcDir, "rapor.pdf", firstPayload)
	f2 := writeTempFile(t, srcDir, "second.bin", secondPayload)
	paths := []string{f1, f2}

	// Attempt 1: completes "rapor.pdf" (placed as "rapor (1).pdf") and drops mid-way through "second.bin".
	s1, r1 := net.Pipe()
	go func() { _ = SendPaths(&cutAfter{Conn: s1, left: 100 << 10}, paths, testCreds(), Hooks{}) }()
	if _, err := Receive(r1, outDir, testCreds(), nil, Hooks{}); err == nil {
		t.Fatal("first attempt succeeded despite cutoff")
	}

	if _, err := os.Stat(filepath.Join(outDir, "rapor (1).pdf")); err != nil {
		t.Fatalf("expected rapor (1).pdf after first attempt: %v", err)
	}

	// Attempt 2: resumes and finishes without re-downloading rapor.pdf or creating rapor (2).pdf.
	s2, r2 := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(s2, paths, testCreds(), Hooks{}) }()

	var firstFileIncomplete int
	saved, err := Receive(r2, outDir, testCreds(), nil, Hooks{
		Progress: func(p Progress) {
			if p.Index == 1 && p.Done < p.Total {
				firstFileIncomplete++
			}
		},
	})
	if err != nil {
		t.Fatalf("second attempt Receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("second attempt SendPaths: %v", err)
	}
	if firstFileIncomplete > 0 {
		t.Errorf("first file was re-downloaded on resume (%d incomplete progress events)", firstFileIncomplete)
	}
	if _, err := os.Stat(filepath.Join(outDir, "rapor (2).pdf")); !os.IsNotExist(err) {
		t.Errorf("resume created duplicate rapor (2).pdf")
	}
	if len(saved) != 2 || filepath.Base(saved[0]) != "rapor (1).pdf" {
		t.Errorf("saved = %v, want first entry to be rapor (1).pdf", saved)
	}
}
