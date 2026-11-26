package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"puresend/internal/p2p"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/multiformats/go-multiaddr"
)

// collect forwards every event a node emits, so a test can wait for the
// ones it cares about in order.
func collect(node *p2p.Node) <-chan p2p.Event {
	out := make(chan p2p.Event, 256)
	go func() {
		for {
			select {
			case <-node.Done():
				return
			case ev := <-node.Events():
				out <- ev
			}
		}
	}()
	return out
}

// waitForEvent returns the first event of type T, failing the test if it
// does not arrive in time.
func waitForEvent[T p2p.Event](t *testing.T, events <-chan p2p.Event, timeout time.Duration) T {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-events:
			if e, ok := ev.(T); ok {
				return e
			}
		case <-deadline:
			var zero T
			t.Fatalf("no %T within %s", zero, timeout)
			return zero
		}
	}
}

// TestRoomSurvivesAServerRestart is the ordinary day behind a tunnel:
// cloudflared restarts, or the server is redeployed. The relay slot and
// the room table go with the connection. The sender used to sit on
// "waiting" forever while its friend was told the code did not exist; now
// it notices, reconnects, and puts the same code back.
func TestRoomSurvivesAServerRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow server restart test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The server's identity must survive the restart — that is the whole
	// point of the key file — and so must its address.
	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _, serverAddr := startWSServer(t, priv, "/ip4/127.0.0.1/tcp/0/ws")
	port, err := first.Addrs()[0].ValueForProtocol(multiaddr.P_TCP)
	if err != nil {
		t.Fatal(err)
	}

	sender, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	events := collect(sender)

	want := []byte("yeniden bağlandıktan sonra da gelmeli")
	src := filepath.Join(t.TempDir(), "belge.txt")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	room, err := sender.Host(ctx, []string{src})
	if err != nil {
		t.Fatal(err)
	}
	waitForEvent[p2p.PreparedEvent](t, events, 10*time.Second)

	// The server goes away...
	first.Close()
	waitForEvent[p2p.ServerLostEvent](t, events, 10*time.Second)

	// ...and comes back with an empty room table.
	_, registry, _ := startWSServer(t, priv, "/ip4/127.0.0.1/tcp/"+port+"/ws")
	waitForEvent[p2p.ServerBackEvent](t, events, 30*time.Second)
	if got := registry.ActiveRooms(); got != 1 {
		t.Fatalf("the restarted server holds %d rooms, want the sender's one back", got)
	}

	// And the friend, arriving now, gets the files with the same code.
	receiver, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	outDir := t.TempDir()
	recvDone := drain(t, receiver)
	go receiver.Fetch(ctx, room, outDir)
	if d := <-recvDone; d.Err != nil {
		t.Fatalf("receive after the restart: %v", d.Err)
	}
	if got, _ := os.ReadFile(filepath.Join(outDir, "belge.txt")); !bytes.Equal(got, want) {
		t.Error("the file did not arrive intact")
	}
}

// TestServerListRescuesADeadAddress covers the address baked into every
// released copy going stale: the client asks the published list where the
// meeting point went.
func TestServerListRescuesADeadAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, _, live := newWSServer(t)
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "# PureSend meeting points\n%s\n", live)
	}))
	defer list.Close()

	// Nothing listens on port 1; the peer ID is a real one, just not ours.
	dead := "/ip4/127.0.0.1/tcp/1/ws/p2p/12D3KooWKKqpYTw3D8arNmcNG7ZK1mPfSH2cQ7ohZqHBmYN6eEAn"

	node, err := p2p.New(ctx, []string{dead}, p2p.WithServerList(list.URL))
	if err != nil {
		t.Fatalf("the server list did not rescue a dead address: %v", err)
	}
	node.Close()

	// A copy built with no address at all, only the list, works too.
	node, err = p2p.New(ctx, nil, p2p.WithServerList(list.URL))
	if err != nil {
		t.Fatalf("a list-only client could not connect: %v", err)
	}
	node.Close()

	// And without a list, a dead address is still the plain error it was.
	if _, err := p2p.New(ctx, []string{dead}); err == nil || !strings.Contains(err.Error(), "meeting point") {
		t.Errorf("a dead address without a list got %v", err)
	}
}

// TestSecondReceiverIsToldTheRoomIsBusy: someone else with the right code
// arriving while a transfer runs is told so plainly, instead of getting a
// reset connection that reads like a network fault.
func TestSecondReceiverIsToldTheRoomIsBusy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	_, _, serverAddr := newWSServer(t)
	sender, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	sendDone := drain(t, sender)

	src := filepath.Join(t.TempDir(), "belge.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	room, err := sender.Host(ctx, []string{src})
	if err != nil {
		t.Fatal(err)
	}

	// The first receiver takes the room and sits on the approval screen.
	first, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstEvents := collect(first)
	go first.Fetch(ctx, room, t.TempDir())
	approval := waitForEvent[p2p.ManifestEvent](t, firstEvents, 30*time.Second)

	// The second one, with the same code, is turned away.
	second, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondDone := drain(t, second)
	go second.Fetch(ctx, room, t.TempDir())
	if d := <-secondDone; d.Err == nil || !strings.Contains(d.Err.Error(), "already sending") {
		t.Fatalf("the second receiver got %v, want to be told the room is busy", d.Err)
	}

	// The first one is unaffected.
	approval.Reply <- true
	if d := waitForEvent[p2p.DoneEvent](t, firstEvents, 30*time.Second); d.Err != nil {
		t.Fatalf("the first receiver failed: %v", d.Err)
	}
	if d := <-sendDone; d.Err != nil {
		t.Fatalf("send failed: %v", d.Err)
	}
}

// TestLookupFallsBackToBackupServer verifies that when a sender hosts on a
// secondary server while the primary server is also reachable, a receiver
// configured with both servers falls back past ErrRoomNotFound on the primary
// and completes the transfer through the secondary.
func TestLookupFallsBackToBackupServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, _, primaryAddr := newWSServer(t)
	_, _, backupAddr := newWSServer(t)

	// Sender hosts on backupAddr (simulating failover while primary had blipped).
	sender, err := p2p.New(ctx, []string{backupAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	sendDone := drain(t, sender)

	want := []byte("yedek sunucu üzerinden bulundu")
	src := filepath.Join(t.TempDir(), "yedek.txt")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	room, err := sender.Host(ctx, []string{src})
	if err != nil {
		t.Fatal(err)
	}

	// Receiver dials primaryAddr first, sees not_found there, and checks backupAddr.
	receiver, err := p2p.New(ctx, []string{primaryAddr, backupAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	outDir := t.TempDir()
	recvDone := drain(t, receiver)
	go receiver.Fetch(ctx, room, outDir)

	if d := <-recvDone; d.Err != nil {
		t.Fatalf("receiver did not find room on backup server: %v", d.Err)
	}
	if d := <-sendDone; d.Err != nil {
		t.Fatalf("sender failed: %v", d.Err)
	}
	if got, _ := os.ReadFile(filepath.Join(outDir, "yedek.txt")); !bytes.Equal(got, want) {
		t.Error("received payload does not match sent payload")
	}
}
