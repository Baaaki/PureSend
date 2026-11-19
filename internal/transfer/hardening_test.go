package transfer

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/schollz/pake/v3"
)

// shortTimeouts makes every deadline expire quickly, so the tests that
// wait for one to fire do not wait for minutes.
func shortTimeouts(t *testing.T) {
	t.Helper()
	saved := timeouts
	timeouts.auth = 300 * time.Millisecond
	timeouts.offerGap = 2 * time.Second
	timeouts.heartbeat = 50 * time.Millisecond
	timeouts.approval = 300 * time.Millisecond
	timeouts.idle = 300 * time.Millisecond
	timeouts.linger = 300 * time.Millisecond
	t.Cleanup(func() { timeouts = saved })
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestManifestFieldsAreValidated covers the fields safeJoin never saw. The
// digest names the partial download a resume continues from, so a digest
// that is really a path — "../../../../home/ali/x" — used to create,
// truncate or delete a file outside the target folder.
func TestManifestFieldsAreValidated(t *testing.T) {
	payload := []byte("gotcha")
	good := digestOf(payload)

	for name, f := range map[string]FileInfo{
		"digest that is a path":  {Path: "a.txt", Size: 6, SHA256: "../../../../x"},
		"digest too short":       {Path: "a.txt", Size: 6, SHA256: good[:63]},
		"digest in upper case":   {Path: "a.txt", Size: 6, SHA256: strings.ToUpper(good)},
		"digest with a slash":    {Path: "a.txt", Size: 6, SHA256: good[:62] + "/x"},
		"empty digest":           {Path: "a.txt", Size: 6},
		"negative size":          {Path: "a.txt", Size: -1, SHA256: good},
		"control character name": {Path: "\x1b[2Jtemiz.txt", Size: 6, SHA256: good},
		"bidi override name":     {Path: "foto\u202Egpj.exe", Size: 6, SHA256: good},
		"name too long":          {Path: strings.Repeat("a", maxNameBytes+1), Size: 6, SHA256: good},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			outDir := filepath.Join(parent, "out")
			m := Manifest{Files: []FileInfo{f}}

			sender, receiver := net.Pipe()
			go fakeSend(t, sender, m, payload)

			confirmed := false
			confirm := func(Manifest) bool { confirmed = true; return true }
			if _, err := Receive(receiver, outDir, testCreds(), confirm, Hooks{}); err == nil {
				t.Fatal("Receive accepted the manifest")
			}
			if confirmed {
				t.Error("an invalid manifest reached the user's approval screen")
			}
			// Nothing at all may have been written, inside or outside.
			entries, _ := os.ReadDir(parent)
			for _, e := range entries {
				t.Errorf("something was written for a refused manifest: %s", e.Name())
			}
		})
	}
}

// TestManifestTotalCannotOverflow guards the sum the progress bar and the
// relay warning are computed from.
func TestManifestTotalCannotOverflow(t *testing.T) {
	d := digestOf(nil)
	m := Manifest{Files: []FileInfo{
		{Path: "a", Size: math.MaxInt64, SHA256: d},
		{Path: "b", Size: 1, SHA256: d},
	}}
	if _, err := checkManifest(t.TempDir(), m); err == nil {
		t.Error("a manifest whose total overflows was accepted")
	}
}

// TestWindowsNames covers the names a Mac or Linux sender can produce and
// Windows cannot store. Without the check they failed at the very last
// step, the rename, with an error nobody could read.
func TestWindowsNames(t *testing.T) {
	for name, want := range map[string]bool{
		"foto.jpg":        true,
		"boşluklu ad.txt": true,
		"con.txt.bak":     false,
		"CON":             false,
		"nul.txt":         false,
		"Com1.log":        false,
		"LPT9":            false,
		"COM¹":            false,
		"console.txt":     true, // only the exact device names are taken
		"aux-notes.md":    true,
		"a<b.txt":         false,
		"soru?.txt":       false,
		"yıldız*.txt":     false,
		`tırnak".txt`:     false,
		"boru|.txt":       false,
		"sonunda nokta.":  false,
		"sonunda boşluk ": false,
		"NUL .txt":        false,
	} {
		if got := windowsCanStore(name); got != want {
			t.Errorf("windowsCanStore(%q) = %v, want %v", name, got, want)
		}
	}

	// And through the real receiving path, as it runs on Windows.
	checkWindowsNames = true
	t.Cleanup(func() { checkWindowsNames = false })

	m := Manifest{Files: []FileInfo{{Path: "klasor/CON.txt", Size: 1, SHA256: digestOf([]byte("x"))}}}
	sender, receiver := net.Pipe()
	go fakeSend(t, sender, m, []byte("x"))
	_, err := Receive(receiver, t.TempDir(), testCreds(), nil, Hooks{})
	if err == nil || !strings.Contains(err.Error(), "cannot store") {
		t.Fatalf("a reserved Windows name got through: %v", err)
	}
}

// TestSilentReceiverTimesOut covers the stall where a peer that
// connects and says nothing used to hold the sender's handler — and with
// it the room — for as long as it liked.
func TestSilentReceiverTimesOut(t *testing.T) {
	shortTimeouts(t)
	src := writeTempFile(t, t.TempDir(), "a.txt", []byte("data"))
	offer, err := NewOffer([]string{src})
	if err != nil {
		t.Fatal(err)
	}

	sender, receiver := net.Pipe()
	defer receiver.Close()

	var claimed atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- Send(sender, offer, testCreds(), SendOptions{
			Claim: func() bool { claimed.Store(true); return true },
		})
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped responding") {
			t.Errorf("a silent receiver ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sender waited for a silent receiver without end")
	}
	if claimed.Load() {
		t.Error("the room was claimed by someone who never proved the code")
	}
}

// TestWrongCodeNeverClaims checks the other half: the room is taken only
// after the handshake, so a stranger without the code cannot keep the
// friend it was meant for out, not even for a moment.
func TestWrongCodeNeverClaims(t *testing.T) {
	src := writeTempFile(t, t.TempDir(), "a.txt", []byte("data"))
	offer, _ := NewOffer([]string{src})

	sender, receiver := net.Pipe()
	var claimed atomic.Bool
	sendErr := make(chan error, 1)
	go func() {
		sendErr <- Send(sender, offer, testCreds(), SendOptions{
			Claim: func() bool { claimed.Store(true); return true },
		})
	}()

	wrong := testCreds()
	wrong.Code = "zebra-zebra-99"
	Receive(receiver, t.TempDir(), wrong, nil, Hooks{})

	if err := <-sendErr; !errors.Is(err, ErrWrongCode) {
		t.Errorf("sender reported %v, want the wrong-code error", err)
	}
	if claimed.Load() {
		t.Error("a receiver with the wrong code claimed the room")
	}
}

// guessingReceiver runs the receiver's half of the exchange with a guessed
// code up to the point where it should prove the key, and then does
// whatever misbehave does instead. It returns the key its guess produced
// and the decoder, so a test can see what the sender says next.
func guessingReceiver(t *testing.T, conn net.Conn, guess string, misbehave func(*json.Encoder)) ([]byte, *json.Decoder) {
	t.Helper()
	creds := testCreds()
	creds.Code = guess
	idA, idB := creds.identities()
	p, err := pake.InitCurveWithIdentities([]byte(creds.Code), roleReceiver, pakeCurve, idA, idB)
	if err != nil {
		t.Fatal(err)
	}
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	if err := enc.Encode(authMsg{PAKE: p.Bytes()}); err != nil {
		t.Fatal(err)
	}
	var theirs authMsg
	if err := dec.Decode(&theirs); err != nil {
		t.Fatal(err)
	}
	if err := p.Update(theirs.PAKE); err != nil {
		t.Fatal(err)
	}
	key, err := p.SessionKey()
	if err != nil {
		t.Fatal(err)
	}
	misbehave(enc)
	return key, dec
}

// TestSenderTagNeedsTheReceiversFirst: the sender's confirmation tag tells
// whoever holds it whether their guess at the code was right. A receiver
// that skips its own proof — sends garbage, or a wrong tag — must not be
// handed the sender's. The garbage case matters most: it is not a wrong
// code, so it never counts against the room, and it used to get the tag
// anyway.
func TestSenderTagNeedsTheReceiversFirst(t *testing.T) {
	for name, misbehave := range map[string]func(*json.Encoder){
		"garbage instead of a tag": func(enc *json.Encoder) { _ = enc.Encode("not a confirmation") },
		"a wrong tag":              func(enc *json.Encoder) { _ = enc.Encode(authMsg{Confirm: []byte("forged")}) },
	} {
		t.Run(name, func(t *testing.T) {
			sender, attacker := net.Pipe()
			defer attacker.Close()
			go func() {
				defer sender.Close()
				dec, enc := json.NewDecoder(sender), json.NewEncoder(sender)
				_, _ = authenticate(roleSender, testCreds(),
					func(m *authMsg) error { return dec.Decode(m) },
					func(m authMsg) error { return enc.Encode(m) }, nil)
			}()

			// The right code: the worst case, where the tag would confirm it.
			key, dec := guessingReceiver(t, attacker, testCode, misbehave)
			var reply authMsg
			if err := dec.Decode(&reply); err == nil && len(reply.Confirm) > 0 {
				if hmac.Equal(reply.Confirm, confirmTag(key, confirmSenderLabel)) {
					t.Fatal("the sender confirmed the code to a receiver that never proved it")
				}
				t.Fatalf("the sender sent a tag to a receiver that never proved the key: %x", reply.Confirm)
			}
		})
	}
}

// TestRefusedGuessIsAnsweredAsWrong: a sender whose room has had all the
// guesses it allows refuses to judge any more proofs. A refused proof must
// get the same answer as a wrong one — even from a receiver that holds the
// code, since confirming it would be one guess more than the room allows.
func TestRefusedGuessIsAnsweredAsWrong(t *testing.T) {
	src := writeTempFile(t, t.TempDir(), "a.txt", []byte("data"))
	offer, _ := NewOffer([]string{src})

	sender, receiver := net.Pipe()
	var shown, holds bool
	sendErr := make(chan error, 1)
	go func() {
		sendErr <- Send(sender, offer, testCreds(), SendOptions{
			Judge: func(proves func() bool) bool {
				shown, holds = true, proves()
				return false
			},
		})
	}()

	outDir := filepath.Join(t.TempDir(), "out")
	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); !errors.Is(err, ErrWrongCode) {
		t.Errorf("receiver got %v, want ErrWrongCode", err)
	}
	if err := <-sendErr; !errors.Is(err, ErrWrongCode) {
		t.Errorf("sender got %v, want ErrWrongCode", err)
	}
	if !shown || !holds {
		t.Error("the judge was not handed the receiver's proof")
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Error("a refused receiver created its download folder")
	}
}

// TestBusyRoomTurnsAway checks what a second receiver with the right code
// hears while the room is taken: a clear "busy", not a reset connection
// that reads like a network fault.
func TestBusyRoomTurnsAway(t *testing.T) {
	src := writeTempFile(t, t.TempDir(), "a.txt", []byte("data"))
	offer, _ := NewOffer([]string{src})

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() {
		sendErr <- Send(sender, offer, testCreds(), SendOptions{Claim: func() bool { return false }})
	}()

	outDir := filepath.Join(t.TempDir(), "out")
	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); !errors.Is(err, ErrBusy) {
		t.Errorf("receiver got %v, want ErrBusy", err)
	}
	if err := <-sendErr; !errors.Is(err, ErrBusy) {
		t.Errorf("sender got %v, want ErrBusy", err)
	}
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Error("a turned-away receiver created its download folder")
	}
}

// TestApprovalWaitIsBounded checks that a receiver who never answers the
// manifest releases the sender after a while.
func TestApprovalWaitIsBounded(t *testing.T) {
	shortTimeouts(t)
	src := writeTempFile(t, t.TempDir(), "a.txt", []byte("data"))
	offer, _ := NewOffer([]string{src})

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- Send(sender, offer, testCreds(), SendOptions{}) }()

	release := make(chan struct{})
	received := make(chan struct{})
	outDir := t.TempDir()
	go func() {
		defer close(received)
		Receive(receiver, outDir, testCreds(), func(Manifest) bool {
			<-release // a person who walked away from the screen
			return false
		}, Hooks{})
	}()
	// Cleanups run last-in first-out: this one finishes the receiver before
	// shortTimeouts puts the real deadlines back.
	t.Cleanup(func() { close(release); <-received })

	select {
	case err := <-sendErr:
		if err == nil || !strings.Contains(err.Error(), "no answer from receiver") {
			t.Errorf("sender ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sender waited for an answer without end")
	}
}

// TestReceiverHearsFromAPreparingSender checks the notes a sender sends
// while it is still reading a large folder: the receiver learns it is
// alive and how far along it is, instead of timing out or staring at
// "preparing" with nothing to go on.
func TestReceiverHearsFromAPreparingSender(t *testing.T) {
	shortTimeouts(t)
	data := []byte("data")
	src := writeTempFile(t, t.TempDir(), "a.txt", data)
	offer, _ := NewOffer([]string{src})
	// Mark the offer as being read without reading it yet, so the test
	// decides when the read finishes.
	offer.startOnce.Do(func() {})

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- Send(sender, offer, testCreds(), SendOptions{}) }()

	var notes atomic.Int32
	hooks := Hooks{Waiting: func(done, total int) {
		if notes.Add(1) == 3 {
			go offer.read(context.Background(), Hooks{})
		}
	}}
	outDir := t.TempDir()
	saved, err := Receive(receiver, outDir, testCreds(), nil, hooks)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if notes.Load() < 3 {
		t.Errorf("the receiver heard %d progress notes, want at least 3", notes.Load())
	}
	if got, _ := os.ReadFile(saved[0]); !bytes.Equal(got, data) {
		t.Error("the file did not arrive intact")
	}
}

// TestOfferNoticesChangedFiles checks that reading the files ahead of time
// cannot hand out a stale digest: a file edited after it was read is read
// again before it is offered.
func TestOfferNoticesChangedFiles(t *testing.T) {
	src := writeTempFile(t, t.TempDir(), "notlar.txt", []byte("first draft"))
	offer, _ := NewOffer([]string{src})
	offer.Start(context.Background(), Hooks{})
	<-offer.Ready()

	// Same size, new content, a different modification time.
	edited := []byte("final draft")
	if err := os.WriteFile(src, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- Send(sender, offer, testCreds(), SendOptions{}) }()
	saved, err := Receive(receiver, t.TempDir(), testCreds(), nil, Hooks{})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got, _ := os.ReadFile(saved[0]); !bytes.Equal(got, edited) {
		t.Errorf("received %q, want the edited %q", got, edited)
	}
}

// TestVanishedFileIsASourceError checks that a file deleted after it was
// chosen is reported as the sender's problem — something retrying cannot
// fix — and that the receiver is told why instead of seeing a dead link.
func TestVanishedFileIsASourceError(t *testing.T) {
	src := writeTempFile(t, t.TempDir(), "gidecek.txt", []byte("data"))
	offer, _ := NewOffer([]string{src})
	offer.Start(context.Background(), Hooks{})
	<-offer.Ready()
	os.Remove(src)

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- Send(sender, offer, testCreds(), SendOptions{}) }()

	_, err := Receive(receiver, t.TempDir(), testCreds(), nil, Hooks{})
	if err == nil || !strings.Contains(err.Error(), "could not prepare") {
		t.Errorf("receiver got %v", err)
	}
	if err := <-sendErr; !IsSourceError(err) {
		t.Errorf("sender got %v, want a source error", err)
	}
}

// TestRemoteErrorTextIsCleaned checks that a message the other side
// chose cannot carry escape sequences into the sender's terminal.
func TestRemoteErrorTextIsCleaned(t *testing.T) {
	src := writeTempFile(t, t.TempDir(), "a.txt", []byte("data"))
	offer, _ := NewOffer([]string{src})

	sender, receiver := net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- Send(sender, offer, testCreds(), SendOptions{}) }()

	// A hostile receiver: proves the code, then declines with a message
	// built to clear the screen.
	enc := json.NewEncoder(receiver)
	dec := json.NewDecoder(receiver)
	if _, err := authenticate(roleReceiver, testCreds(),
		func(m *authMsg) error { return dec.Decode(m) },
		func(m authMsg) error { return enc.Encode(m) },
		nil,
	); err != nil {
		t.Fatal(err)
	}
	var msg offerMsg
	for msg.Manifest == nil {
		if err := dec.Decode(&msg); err != nil {
			t.Fatal(err)
		}
	}
	enc.Encode(ack{OK: false, Error: "\x1b[2J\x1b[Hher şey yolunda"})
	receiver.Close()

	err := <-sendErr
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("the sender's error carries the escape sequence: %q", err)
	}
}

// TestResumeWithIdenticalFiles is the folder holding the same photo twice.
// Both copies used to share one partial download: the first finished and
// took it away, and the second then continued "from where it stopped" in
// a file that no longer existed — padded with zeroes, failing its checksum
// on every retry.
func TestResumeWithIdenticalFiles(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	photo := bytes.Repeat([]byte("aynı fotoğraf "), 20_000) // ~300 KB
	writeTempFile(t, srcDir, "album/bir.jpg", photo)
	writeTempFile(t, srcDir, "album/iki.jpg", photo)
	root := filepath.Join(srcDir, "album")

	// First attempt dies partway through the first copy.
	sender, receiver := net.Pipe()
	go SendPaths(&cutAfter{Conn: sender, left: 100 << 10}, []string{root}, testCreds(), Hooks{})
	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil {
		t.Fatal("the interrupted transfer reported success")
	}

	sender, receiver = net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{root}, testCreds(), Hooks{}) }()
	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err != nil {
		t.Fatalf("the resumed transfer failed: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("resumed Send: %v", err)
	}
	for _, name := range []string{"bir.jpg", "iki.jpg"} {
		got, err := os.ReadFile(filepath.Join(outDir, "album", name))
		if err != nil || !bytes.Equal(got, photo) {
			t.Errorf("%s did not arrive intact: %v", name, err)
		}
	}
}

// TestResumeKeepsFinishedFiles checks that a retry does not fetch again
// what the earlier attempt already finished — which used to arrive a
// second time as "bir (1).jpg" next to the first.
func TestResumeKeepsFinishedFiles(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	// Random, so compression cannot shrink the transfer to below the cut.
	first, second := make([]byte, 50_000), make([]byte, 300_000)
	_, _ = rand.Read(first)
	_, _ = rand.Read(second)
	writeTempFile(t, srcDir, "album/a.bin", first)
	writeTempFile(t, srcDir, "album/b.bin", second)
	root := filepath.Join(srcDir, "album")

	// Cut after the first file is complete but well before the second is.
	sender, receiver := net.Pipe()
	go SendPaths(&cutAfter{Conn: sender, left: 150 << 10}, []string{root}, testCreds(), Hooks{})
	Receive(receiver, outDir, testCreds(), nil, Hooks{})
	if got, _ := os.ReadFile(filepath.Join(outDir, "album", "a.bin")); !bytes.Equal(got, first) {
		t.Fatal("setup: the first file should have been finished by the first attempt")
	}
	if _, err := os.Stat(filepath.Join(outDir, "album", "b.bin")); !os.IsNotExist(err) {
		t.Fatal("setup: the second file should not have been finished by the first attempt")
	}

	var resumedFrom []int64
	hooks := Hooks{Progress: func(p Progress) {
		if p.Index > len(resumedFrom) {
			resumedFrom = append(resumedFrom, p.Done)
		}
	}}
	sender, receiver = net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{root}, testCreds(), Hooks{}) }()
	saved, err := Receive(receiver, outDir, testCreds(), nil, hooks)
	if err != nil {
		t.Fatalf("the resumed transfer failed: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("resumed Send: %v", err)
	}

	if len(resumedFrom) == 0 || resumedFrom[0] != int64(len(first)) {
		t.Errorf("the finished file was fetched again (started at %v)", resumedFrom)
	}
	entries, _ := os.ReadDir(filepath.Join(outDir, "album"))
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("album holds %v, want just a.bin and b.bin", names)
	}
	if got, _ := os.ReadFile(saved[1]); !bytes.Equal(got, second) {
		t.Error("the second file did not arrive intact")
	}
}

// fakeHome points the home folder at a fresh temporary one for the test.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// TestDestinationIsNotTheHomeFolder: straight into the home folder, a
// sender could plant ".config/autostart/x.desktop" or
// ".ssh/authorized_keys" — nothing overwritten, and something runs, or
// someone gets in, at the next login.
func TestDestinationIsNotTheHomeFolder(t *testing.T) {
	home := fakeHome(t)

	for _, bad := range []string{home, filepath.Dir(home), filepath.VolumeName(home) + string(filepath.Separator)} {
		if err := CheckDestination(bad); !errors.Is(err, ErrUnsafeDestination) {
			t.Errorf("CheckDestination(%q) = %v, want ErrUnsafeDestination", bad, err)
		}
	}
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, link); err == nil {
		if err := CheckDestination(link); !errors.Is(err, ErrUnsafeDestination) {
			t.Errorf("a link to the home folder got through: %v", err)
		}
	}
	for _, good := range []string{
		filepath.Join(home, "Downloads", "PureSend"),
		filepath.Join(home, "Desktop"),
		t.TempDir(),
	} {
		if err := CheckDestination(good); err != nil {
			t.Errorf("CheckDestination(%q) = %v, want nil", good, err)
		}
	}

	// And through the real receiving path: refused before the handshake,
	// with nothing written.
	payload := []byte("[Desktop Entry]")
	m := Manifest{Files: []FileInfo{{Path: ".config/autostart/x.desktop", Size: int64(len(payload)), SHA256: digestOf(payload)}}}
	sender, receiver := net.Pipe()
	go fakeSend(t, sender, m, payload)
	if _, err := Receive(receiver, home, testCreds(), nil, Hooks{}); !errors.Is(err, ErrUnsafeDestination) {
		t.Fatalf("receiving into the home folder got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); !os.IsNotExist(err) {
		t.Error("something was written into the home folder")
	}
}

// TestNoWritingThroughLinks: a link already in the destination — "docs"
// pointing somewhere else entirely — must not carry a file out of it.
func TestNoWritingThroughLinks(t *testing.T) {
	outDir, elsewhere := t.TempDir(), t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(outDir, "docs")); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}

	payload := []byte("gotcha")
	m := Manifest{Files: []FileInfo{{Path: "docs/x.txt", Size: int64(len(payload)), SHA256: digestOf(payload)}}}
	sender, receiver := net.Pipe()
	go fakeSend(t, sender, m, payload)
	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("a file went through a link: %v", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("something landed where the link points: %v", entries)
	}
}

// TestOversizedChunkRejected: a compressed chunk that runs past the end of
// the file it belongs to is refused as it arrives, not after the whole lie
// has been written to disk and failed its digest.
func TestOversizedChunkRejected(t *testing.T) {
	data := bytes.Repeat([]byte("more than promised "), 100)
	m := Manifest{
		Files:      []FileInfo{{Path: "short.txt", Size: 10, SHA256: digestOf(data[:10])}},
		Compressed: true,
	}
	comp := CompressChunk(nil, data)
	frame := make([]byte, 4, 4+len(comp))
	binary.BigEndian.PutUint32(frame, uint32(len(comp))|compressionFlag)
	frame = append(frame, comp...)

	outDir := t.TempDir()
	sender, receiver := net.Pipe()
	go fakeSend(t, sender, m, frame)
	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil || !strings.Contains(err.Error(), "more of") {
		t.Fatalf("an oversized chunk got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "short.txt")); !os.IsNotExist(err) {
		t.Error("the oversized file was saved")
	}
}

// TestSenderStopsAtTheDeclaredSize: a file that grew after it was read
// must not put its extra bytes on the wire, where the receiver would take
// them for the next file.
func TestSenderStopsAtTheDeclaredSize(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 3*chunkSize+123)
	src := writeTempFile(t, t.TempDir(), "growing.log", content)
	declared := int64(chunkSize + 7) // what the manifest said, before it grew

	for _, compressed := range []bool{false, true} {
		var wire bytes.Buffer
		info := FileInfo{Path: "growing.log", Size: declared}
		if err := sendFile(&wire, deadlines{}, src, info, 0, nil, compressed); err != nil {
			t.Fatalf("compressed=%v: sendFile: %v", compressed, err)
		}
		got := wire.Bytes()
		if compressed {
			var plain []byte
			for len(got) > 0 {
				hdr := binary.BigEndian.Uint32(got[:4])
				n := int(hdr &^ compressionFlag)
				chunk := got[4 : 4+n]
				if hdr&compressionFlag != 0 {
					var err error
					if chunk, err = DecompressChunk(nil, chunk, 2*chunkSize); err != nil {
						t.Fatal(err)
					}
				}
				plain = append(plain, chunk...)
				got = got[4+n:]
			}
			got = plain
		}
		if int64(len(got)) != declared {
			t.Errorf("compressed=%v: %d bytes on the wire, want the declared %d", compressed, len(got), declared)
		}
	}
}

// TestPlaceNeverReplaces covers the moment between finding a free name and
// taking it: whatever is at the target by then — a file, or a link that
// points nowhere — stays as it is.
func TestPlaceNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "doc.txt")
	writeTempFile(t, dir, "doc.txt", []byte("existing"))
	dangling := filepath.Join(dir, "doc (1).txt")
	want := filepath.Join(dir, "doc (2).txt")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Logf("no symbolic links here, checking files only: %v", err)
		dangling, want = "", filepath.Join(dir, "doc (1).txt")
	}
	src := writeTempFile(t, dir, "doc.part", []byte("new"))

	final, err := place(src, target)
	if err != nil {
		t.Fatal(err)
	}
	if final != want {
		t.Errorf("placed at %s, want %s", final, want)
	}
	if got, _ := os.ReadFile(target); string(got) != "existing" {
		t.Error("the existing file was replaced")
	}
	if dangling != "" {
		if st, err := os.Lstat(dangling); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Error("the dangling link was replaced")
		}
	}
	if got, _ := os.ReadFile(final); string(got) != "new" {
		t.Errorf("the new file at %s holds %q", final, got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("the partial file was left behind")
	}
}

// TestResumeRevealsNoOtherFiles: the offsets a receiver sends back say
// which files it already has. Only files an interrupted transfer finished
// may be claimed — otherwise a sender could offer a file by its digest and
// learn from the answer whether the receiver's folder holds it.
func TestResumeRevealsNoOtherFiles(t *testing.T) {
	outDir := t.TempDir()
	private := []byte("a file that was already in the folder")
	writeTempFile(t, outDir, "private.txt", private)
	m := Manifest{Files: []FileInfo{{Path: "private.txt", Size: int64(len(private)), SHA256: digestOf(private)}}}

	sender, receiver := net.Pipe()
	offsets := make(chan []int64, 1)
	go func() {
		defer sender.Close()
		enc, dec := json.NewEncoder(sender), json.NewDecoder(sender)
		if _, err := authenticate(roleSender, testCreds(),
			func(m *authMsg) error { return dec.Decode(m) },
			func(m authMsg) error { return enc.Encode(m) }, nil); err != nil {
			return
		}
		_ = enc.Encode(offerMsg{Manifest: &m})
		var goAhead ack
		if dec.Decode(&goAhead) != nil {
			return
		}
		offsets <- goAhead.Offsets
		if len(goAhead.Offsets) == 1 && goAhead.Offsets[0] == 0 {
			_, _ = sender.Write(private)
		}
		var done ack
		_ = dec.Decode(&done)
	}()

	saved, err := Receive(receiver, outDir, testCreds(), nil, Hooks{})
	if got := <-offsets; len(got) != 1 || got[0] != 0 {
		t.Fatalf("the receiver told the sender it had %v of a file it never received", got)
	}
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	// Downloaded all the same, and recognised as the file already there
	// rather than saved a second time next to it.
	if len(saved) != 1 || saved[0] != filepath.Join(outDir, "private.txt") {
		t.Errorf("saved %v, want the existing private.txt", saved)
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the folder holds %v, want just private.txt", names)
	}
}

func FuzzSafeJoin(f *testing.F) {
	seeds := []string{
		"belge.txt",
		"klasor/resim.png",
		"a/b/c/d/e.txt",
		"../../etc/passwd",
		"../../../.bashrc",
		"/root/secret",
		"C:\\Windows\\explorer.exe",
		"CON",
		"NUL.txt",
		"AUX",
		"COM1",
		"a/./b",
		"a//b",
		"dosya.",
		"dosya ",
		"foto\u202Egpj.exe",
		"\x1b[2Jtemiz.txt",
		".puresend-partial/sinsi.bin",
		".PURESEND-PARTIAL/sinsi.bin",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	outDir := filepath.Join(os.TempDir(), "fuzz-out")
	f.Fuzz(func(t *testing.T, rel string) {
		target, err := safeJoin(outDir, rel)
		if err != nil {
			return
		}

		// Invariant 1: Target must strictly be inside outDir and not escape it.
		cleanOutDir := filepath.Clean(outDir)
		cleanTarget := filepath.Clean(target)
		if cleanTarget != cleanOutDir && !strings.HasPrefix(cleanTarget, cleanOutDir+string(filepath.Separator)) {
			t.Fatalf("safeJoin(%q) produced target outside outDir: target=%q, outDir=%q", rel, cleanTarget, cleanOutDir)
		}

		diff, relErr := filepath.Rel(outDir, target)
		if relErr != nil || diff == ".." || strings.HasPrefix(diff, ".."+string(filepath.Separator)) {
			t.Fatalf("safeJoin(%q) escaped outDir: target=%q, diff=%q", rel, target, diff)
		}

		// Invariant 2: Target must never begin with reserved partial download
		// directory, in any case — on macOS and Windows those are one folder.
		first, _, _ := strings.Cut(filepath.ToSlash(diff), "/")
		if strings.EqualFold(first, partialDir) {
			t.Fatalf("safeJoin(%q) allowed reserved partialDir in target=%q", rel, target)
		}
	})
}

func FuzzValidDigest(f *testing.F) {
	seeds := []string{
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",  // sha256 of empty string
		"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",  // sha256 of "abc"
		"E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855",  // uppercase
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85",   // 63 chars
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8555", // 65 chars
		"",
		"../../../../x",
		"g3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // 'g' is invalid hex
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		got := validDigest(s)
		if got {
			if len(s) != 64 {
				t.Fatalf("validDigest(%q) = true, but length is %d (want 64)", s, len(s))
			}
			for i := 0; i < len(s); i++ {
				c := s[i]
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Fatalf("validDigest(%q) = true, but contains non-hex char %c", s, c)
				}
			}
		}
	})
}

// TestMalformedPAKEPayloadsDoNotPanic guards against remote nil-pointer
// panics in schollz/pake when a peer sends JSON "null", "{}", or missing
// elliptic curve coordinates.
func TestMalformedPAKEPayloadsDoNotPanic(t *testing.T) {
	payloads := map[string][]byte{
		"null":       []byte("null"),
		"empty obj":  []byte("{}"),
		"missing Xv": []byte(`{"Role":0,"Xᵤ":1}`),
		"missing Yv": []byte(`{"Role":1,"Yᵤ":1}`),
		"wrong role": []byte(`{"Role":1,"Xᵤ":1,"Xᵥ":2}`),
	}

	for name, raw := range payloads {
		t.Run("sender/"+name, func(t *testing.T) {
			src := writeTempFile(t, t.TempDir(), "a.txt", []byte("data"))
			offer, _ := NewOffer([]string{src})
			sender, attacker := net.Pipe()
			defer attacker.Close()

			sendErr := make(chan error, 1)
			go func() {
				sendErr <- Send(sender, offer, testCreds(), SendOptions{})
			}()

			if err := json.NewEncoder(attacker).Encode(authMsg{PAKE: raw}); err != nil {
				t.Fatal(err)
			}
			if err := <-sendErr; !errors.Is(err, ErrWrongCode) {
				t.Errorf("sender got %v, want ErrWrongCode", err)
			}
		})

		t.Run("receiver/"+name, func(t *testing.T) {
			sender, receiver := net.Pipe()
			defer sender.Close()

			go func() {
				var first authMsg
				_ = json.NewDecoder(sender).Decode(&first)
				_ = json.NewEncoder(sender).Encode(authMsg{PAKE: raw})
			}()

			if _, err := Receive(receiver, t.TempDir(), testCreds(), nil, Hooks{}); !errors.Is(err, ErrWrongCode) {
				t.Errorf("receiver got %v, want ErrWrongCode", err)
			}
		})
	}
}

// TestEmptyCompressedChunkRejected ensures a valid 0-byte DEFLATE block
// cannot spin receiveFile forever while refreshing the idle deadline.
func TestEmptyCompressedChunkRejected(t *testing.T) {
	payload := []byte("hello world")
	m := Manifest{
		Files:      []FileInfo{{Path: "a.txt", Size: int64(len(payload)), SHA256: digestOf(payload)}},
		Compressed: true,
	}
	emptyComp := CompressChunk(nil, nil)
	frame := make([]byte, 4, 4+len(emptyComp))
	binary.BigEndian.PutUint32(frame, uint32(len(emptyComp))|compressionFlag)
	frame = append(frame, emptyComp...)

	outDir := t.TempDir()
	sender, receiver := net.Pipe()
	go fakeSend(t, sender, m, frame)

	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil || !strings.Contains(err.Error(), "empty chunk") {
		t.Fatalf("empty compressed chunk got %v, want empty chunk error", err)
	}
}

// TestResumeIdenticalFilesAfterFirstFinishes guards against a bug where
// keying .done marks solely by SHA-256 caused the second copy of an
// identical file to be skipped on resume once the first copy had finished.
func TestResumeIdenticalFilesAfterFirstFinishes(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	photo := make([]byte, 120_000)
	_, _ = rand.Read(photo)
	writeTempFile(t, srcDir, "album/bir.jpg", photo)
	writeTempFile(t, srcDir, "album/iki.jpg", photo)
	root := filepath.Join(srcDir, "album")

	// Cut after bir.jpg is complete (~120 KB) and halfway through iki.jpg.
	sender, receiver := net.Pipe()
	go SendPaths(&cutAfter{Conn: sender, left: 180_000}, []string{root}, testCreds(), Hooks{})
	if _, err := Receive(receiver, outDir, testCreds(), nil, Hooks{}); err == nil {
		t.Fatal("the interrupted transfer reported success")
	}
	if _, err := os.Stat(filepath.Join(outDir, "album", "bir.jpg")); err != nil {
		t.Fatalf("setup: bir.jpg should have finished in first attempt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "album", "iki.jpg")); !os.IsNotExist(err) {
		t.Fatalf("setup: iki.jpg should not have finished in first attempt")
	}

	// Resume: both bir.jpg and iki.jpg must end up on disk intact.
	sender, receiver = net.Pipe()
	sendErr := make(chan error, 1)
	go func() { sendErr <- SendPaths(sender, []string{root}, testCreds(), Hooks{}) }()
	saved, err := Receive(receiver, outDir, testCreds(), nil, Hooks{})
	if err != nil {
		t.Fatalf("resumed Receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("resumed Send: %v", err)
	}
	if len(saved) != 2 || saved[0] == saved[1] {
		t.Fatalf("saved = %v, want two distinct paths", saved)
	}
	for _, name := range []string{"bir.jpg", "iki.jpg"} {
		got, err := os.ReadFile(filepath.Join(outDir, "album", name))
		if err != nil || !bytes.Equal(got, photo) {
			t.Errorf("%s missing or corrupted after resume: %v", name, err)
		}
	}
}

// TestHaveAlreadyDoesNotFollowLeafSymlink verifies a leaf symlink sitting at
// target is never followed when checking whether a file already matches.
func TestHaveAlreadyDoesNotFollowLeafSymlink(t *testing.T) {
	outDir, elsewhere := t.TempDir(), t.TempDir()
	secret := []byte("outside secret")
	outside := writeTempFile(t, elsewhere, "secret.txt", secret)
	link := filepath.Join(outDir, "secret.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	info := FileInfo{Path: "secret.txt", Size: int64(len(secret)), SHA256: digestOf(secret)}
	if haveAlready(link, info, func() {}) {
		t.Fatal("haveAlready followed a leaf symlink outside outDir")
	}
}

// TestAlreadyOnDiskAccountsForPartialsAndMarks verifies resume disk space
// estimation subtracts bytes already present in .part files and finished files.
func TestAlreadyOnDiskAccountsForPartialsAndMarks(t *testing.T) {
	outDir := t.TempDir()
	data1 := bytes.Repeat([]byte("a"), 1000)
	data2 := bytes.Repeat([]byte("b"), 2000)
	f1 := FileInfo{Path: "one.bin", Size: 1000, SHA256: digestOf(data1)}
	f2 := FileInfo{Path: "two.bin", Size: 2000, SHA256: digestOf(data2)}
	m := Manifest{Files: []FileInfo{f1, f2}}
	targets, err := checkManifest(outDir, m)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate f1 finished and f2 halfway downloaded.
	writeTempFile(t, outDir, "one.bin", data1)
	if err := os.MkdirAll(filepath.Join(outDir, partialDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finishedMark(outDir, f1.Path, f1.SHA256), []byte("one.bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, partialDir, f2.SHA256+".part"), data2[:750], 0o600); err != nil {
		t.Fatal(err)
	}

	if got := alreadyOnDisk(outDir, m, targets); got != 1750 {
		t.Errorf("alreadyOnDisk = %d, want 1750", got)
	}
}

// TestCollectSymlinkedDirectory verifies passing a top-level symlink to a
// directory still walks the directory's regular files while skipping
// symlinks nested inside it.
func TestCollectSymlinkedDirectory(t *testing.T) {
	realDir := t.TempDir()
	writeTempFile(t, realDir, "inside.txt", []byte("ok"))
	outside := writeTempFile(t, t.TempDir(), "outside.txt", []byte("nope"))
	if err := os.Symlink(outside, filepath.Join(realDir, "nested-link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	linkDir := filepath.Join(t.TempDir(), "shared")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	entries, err := Collect([]string{linkDir})
	if err != nil {
		t.Fatalf("Collect(symlinked dir): %v", err)
	}
	if len(entries) != 1 || entries[0].Rel != "shared/inside.txt" {
		t.Fatalf("Collect = %+v, want single entry shared/inside.txt", entries)
	}
}
