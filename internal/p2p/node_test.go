package p2p

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"puresend/internal/transfer"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/holepunch"
	"golang.org/x/crypto/blake2b"
)

// newTestNode builds a Node around a host that listens nowhere. Enough to
// exercise the shutdown path without touching the network.
func newTestNode(t *testing.T) *Node {
	t.Helper()
	h, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("could not start a test host: %v", err)
	}
	// events is deliberately tiny so a single value fills it.
	ctx, cancel := context.WithCancel(context.Background())
	return &Node{
		host:       h,
		punch:      newPunchWatcher(),
		events:     make(chan Event, 1),
		handshakes: make(chan struct{}, handshakeSlots),
		lost:       make(chan struct{}, 1),
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
}

// TestCloseIsIdempotent guards the two paths that both close the node:
// the user quitting mid-session and the program tearing down on the way
// out. A second Close must not panic on an already-closed channel.
func TestCloseIsIdempotent(t *testing.T) {
	n := newTestNode(t)

	if err := n.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := n.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	select {
	case <-n.Done():
	default:
		t.Error("Done was not closed by Close")
	}
}

// TestEmitDoesNotBlockAfterClose covers the shutdown race: when the user
// quits, the interface stops reading events while a transfer may still be
// unwinding. Without the done case in emit, that transfer's goroutine
// would park forever on a send nobody will ever receive.
func TestEmitDoesNotBlockAfterClose(t *testing.T) {
	n := newTestNode(t)
	defer n.Close()

	// Fill the buffer so the send in emit cannot possibly proceed.
	n.events <- StatusEvent{Text: "filler"}
	n.Close()

	emitted := make(chan struct{})
	go func() {
		n.emit(DoneEvent{}) // blocks forever if emit ignores done
		close(emitted)
	}()

	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked after the node was closed")
	}
}

// TestProgressEventsNeverBlock checks the throttling escape hatch: a slow
// consumer must not be able to stall the transfer itself, because a
// progress event is superseded by the next one anyway.
func TestProgressEventsNeverBlock(t *testing.T) {
	n := newTestNode(t)
	defer n.Close()

	emitted := make(chan struct{})
	go func() {
		// One more than the buffer holds; the overflow is dropped.
		for i := range 3 {
			n.emit(ProgressEvent{Progress: transfer.Progress{
				Name: "tatil.jpg", Done: int64(i), Total: 2,
			}})
		}
		close(emitted)
	}()

	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		t.Fatal("a progress event blocked on a full channel")
	}
}

// TestSplitServers covers the list a released binary is built with: more
// than one meeting point is the whole insurance policy against the single
// address baked into every copy of the program going away.
func TestSplitServers(t *testing.T) {
	for in, want := range map[string]int{
		"":                   0,
		"   ":                0,
		"/ip4/1.2.3.4/tcp/1": 1,
		"/ip4/1.2.3.4/tcp/1, /dns4/b/tcp/443/tls/ws ": 2,
		"a,,b,": 2,
	} {
		if got := SplitServers(in); len(got) != want {
			t.Errorf("SplitServers(%q) = %v, want %d entries", in, got, want)
		}
	}
}

// TestPunchWatcherGivesUp covers the signal that replaced a fixed 20
// second wait: DCUtR reports each failed attempt, and once it has used
// them all there is nothing left to wait for.
func TestPunchWatcherGivesUp(t *testing.T) {
	w := newPunchWatcher()
	p := peer.ID("friend")
	gaveUp := w.watch(p)

	for range holePunchAttempts - 1 {
		w.Trace(&holepunch.Event{Remote: p, Evt: &holepunch.EndHolePunchEvt{Success: false}})
		select {
		case <-gaveUp:
			t.Fatal("gave up while attempts were still left")
		default:
		}
	}

	w.Trace(&holepunch.Event{Remote: p, Evt: &holepunch.EndHolePunchEvt{Success: false}})
	select {
	case <-gaveUp:
	case <-time.After(time.Second):
		t.Fatal("the last failed attempt did not end the wait")
	}
}

// TestPunchWatcherStopsOnProtocolError checks the other give-up path: when
// the two sides cannot even coordinate a hole punch there are no retries,
// so waiting out the timeout would be pure delay.
func TestPunchWatcherStopsOnProtocolError(t *testing.T) {
	w := newPunchWatcher()
	p := peer.ID("friend")
	gaveUp := w.watch(p)

	w.Trace(&holepunch.Event{Remote: p, Evt: &holepunch.ProtocolErrorEvt{Error: "no"}})
	select {
	case <-gaveUp:
	case <-time.After(time.Second):
		t.Fatal("a protocol error did not end the wait")
	}
}

// TestPunchWatcherIgnoresOtherPeers guards against one failing peer
// cutting short the wait for a different, healthy one.
func TestPunchWatcherIgnoresOtherPeers(t *testing.T) {
	w := newPunchWatcher()
	mine := peer.ID("friend")
	gaveUp := w.watch(mine)

	for range holePunchAttempts + 2 {
		w.Trace(&holepunch.Event{Remote: peer.ID("stranger"), Evt: &holepunch.EndHolePunchEvt{}})
	}
	select {
	case <-gaveUp:
		t.Fatal("another peer's failures ended our wait")
	default:
	}

	w.forget(mine)
	// Events after forget must not panic on a closed or missing channel.
	w.Trace(&holepunch.Event{Remote: mine, Evt: &holepunch.ProtocolErrorEvt{}})
}

// TestWaitForDirectStopsWhenTheNodeCloses covers the shutdown path: a user
// who gives up mid-wait must not leave the wait running.
func TestWaitForDirectStopsWhenTheNodeCloses(t *testing.T) {
	n := newTestNode(t)

	returned := make(chan bool, 1)
	go func() { returned <- n.waitForDirect(context.Background(), peer.ID("friend"), time.Minute) }()

	n.Close()
	select {
	case direct := <-returned:
		if direct {
			t.Error("reported a direct connection that does not exist")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait outlived the node")
	}
}

// TestCheckCode covers what the receiving side does with a typed code
// before it asks the server anything: a typo in the shape, or a word the
// program never uses, is caught here instead of costing one of the few
// lookups the server allows.
func TestCheckCode(t *testing.T) {
	if code, err := CheckCode("  KİRAZ liman 42 "); err != nil || code != "kiraz-liman-42" {
		t.Errorf("CheckCode normalized to %q, %v", code, err)
	}
	if _, err := CheckCode("kiraz-liman"); err == nil || !strings.Contains(err.Error(), "not a room code") {
		t.Errorf("a code without its number got %v", err)
	}
	if _, err := CheckCode("kirez-liman-42"); err == nil || !strings.Contains(err.Error(), `"kirez"`) {
		t.Errorf("a misspelt word got %v", err)
	}
}

// TestFetchServerList covers the escape hatch for a server that moved:
// comments and blank lines are ignored, and a broken list is an error
// rather than an empty success.
func TestFetchServerList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server.txt":
			fmt.Fprint(w, "# current meeting points\n\n/dns4/a/tcp/443/tls/ws/p2p/X  # primary\n  /dns4/b/tcp/443/tls/ws/p2p/Y\n")
		case "/empty.txt":
			fmt.Fprint(w, "# nothing yet\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := fetchServerList(context.Background(), srv.URL+"/server.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/dns4/a/tcp/443/tls/ws/p2p/X", "/dns4/b/tcp/443/tls/ws/p2p/Y"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("fetchServerList = %q, want %q", got, want)
	}
	for _, path := range []string{"/empty.txt", "/missing.txt"} {
		if _, err := fetchServerList(context.Background(), srv.URL+path); err == nil {
			t.Errorf("%s was accepted as a server list", path)
		}
	}
}

// TestHandshakePerPeerLimitPreventsStarvation ensures a single remote peer
// cannot occupy all global handshake slots and lock out other receivers.
func TestHandshakePerPeerLimitPreventsStarvation(t *testing.T) {
	n := newTestNode(t)
	defer n.Close()

	attacker := peer.ID("attacker")
	friend := peer.ID("friend")

	for i := range maxHandshakesPerPeer {
		if !n.tryAcquireHandshake(attacker) {
			t.Fatalf("slot %d for attacker refused prematurely", i)
		}
	}
	if n.tryAcquireHandshake(attacker) {
		t.Fatal("attacker exceeded maxHandshakesPerPeer")
	}
	if !n.tryAcquireHandshake(friend) {
		t.Fatal("attacker holding its max slots starved a legitimate peer")
	}
	n.releaseHandshake(friend)
	for range maxHandshakesPerPeer {
		n.releaseHandshake(attacker)
	}
}

// TestKeepRoomExpiresWithoutDisconnect verifies that a waiting room ends
// with ErrRoomExpired after roomTTL even when the server connection never drops.
func TestKeepRoomExpiresWithoutDisconnect(t *testing.T) {
	savedTTL := roomTTL
	roomTTL = 50 * time.Millisecond
	t.Cleanup(func() { roomTTL = savedTTL })

	n := newTestNode(t)
	defer n.Close()
	n.mu.Lock()
	n.room = "kiraz-liman-42"
	n.hostedAt = time.Now()
	n.mu.Unlock()

	go n.keepRoom()

	select {
	case ev := <-n.Events():
		lost, ok := ev.(RoomLostEvent)
		if !ok || !errors.Is(lost.Err, ErrRoomExpired) {
			t.Fatalf("got event %#v, want RoomLostEvent{Err: ErrRoomExpired}", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("room did not expire after roomTTL while connected")
	}
}

// TestWaitForDirectChCatchesPreArmedGiveUp ensures that if DCUtR fails with a
// ProtocolErrorEvt while host.Connect is still finishing, a pre-armed watcher
// returns immediately instead of hanging for the full directWait timeout.
func TestWaitForDirectChCatchesPreArmedGiveUp(t *testing.T) {
	n := newTestNode(t)
	defer n.Close()

	friend := peer.ID("friend")
	gaveUp := n.punch.watch(friend)
	defer n.punch.forget(friend)

	// Simulate DCUtR failing inside Connect before waitForDirectCh starts.
	n.punch.Trace(&holepunch.Event{Remote: friend, Evt: &holepunch.ProtocolErrorEvt{Error: "no addrs"}})

	start := time.Now()
	if n.waitForDirectCh(context.Background(), friend, 10*time.Second, gaveUp) {
		t.Fatal("reported direct connection when none exists")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waitForDirectCh took %s despite pre-armed giveUp", elapsed)
	}
}

// TestHooksThrottleAcrossManySmallFiles verifies that completing hundreds of
// small files in rapid succession throttles intermediate per-file completions
// while still delivering the final 100% progress event.
func TestHooksThrottleAcrossManySmallFiles(t *testing.T) {
	h, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := &Node{
		host:   h,
		punch:  newPunchWatcher(),
		events: make(chan Event, 64),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	hooks := n.hooks()
	const files = 200
	for i := 1; i <= files; i++ {
		hooks.Progress(transfer.Progress{
			Name:         fmt.Sprintf("f%d.txt", i),
			Index:        i,
			Files:        files,
			Done:         10,
			Total:        10,
			OverallDone:  int64(i * 10),
			OverallTotal: int64(files * 10),
		})
	}

	var got []ProgressEvent
	for len(n.events) > 0 {
		got = append(got, (<-n.events).(ProgressEvent))
	}
	if len(got) == 0 || len(got) > 10 {
		t.Fatalf("emitted %d progress events for %d instant files, want 2..10", len(got), files)
	}
	last := got[len(got)-1]
	if last.Index != files || last.OverallDone != last.OverallTotal {
		t.Errorf("final progress event = %+v, want final 100%% event", last)
	}
}

func TestFetchServerListHTTPSAndSignature(t *testing.T) {
	if _, err := fetchServerList(context.Background(), "http://example.com/server.txt"); err == nil {
		t.Fatal("fetchServerList accepted a remote plain http:// URL")
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var keyID [8]byte
	_, _ = rand.Read(keyID[:])
	pubText := base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), keyID[:]...), pub...))

	body := []byte("/dns4/a.example.com/tcp/443/tls/ws/p2p/12D3KooWtest\n")
	digest := blake2b.Sum512(body)
	sig := ed25519.Sign(priv, digest[:])
	trusted := "timestamp:1790000000\tfile:server.txt"
	global := ed25519.Sign(priv, append(append([]byte(nil), sig...), trusted...))
	sigFile := []byte("untrusted comment: sig\n" +
		base64.StdEncoding.EncodeToString(append(append([]byte("ED"), keyID[:]...), sig...)) + "\n" +
		"trusted comment: " + trusted + "\n" +
		base64.StdEncoding.EncodeToString(global) + "\n")

	var serveTampered bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server.txt":
			if serveTampered {
				_, _ = w.Write([]byte("/dns4/evil.example.com/tcp/443/tls/ws/p2p/12D3KooWevil\n"))
				return
			}
			_, _ = w.Write(body)
		case "/server.txt.minisig":
			_, _ = w.Write(sigFile)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	savedKey := serverListPublicKey
	serverListPublicKey = pubText
	t.Cleanup(func() { serverListPublicKey = savedKey })

	got, err := fetchServerList(context.Background(), srv.URL+"/server.txt")
	if err != nil || len(got) != 1 {
		t.Fatalf("valid signed server list failed: %v (%v)", err, got)
	}

	serveTampered = true
	if _, err := fetchServerList(context.Background(), srv.URL+"/server.txt"); err == nil {
		t.Fatal("tampered server list passed minisign verification")
	}
}
