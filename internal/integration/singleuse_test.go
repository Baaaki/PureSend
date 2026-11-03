package integration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"puresend/internal/p2p"
	"puresend/internal/rendezvous"
	"puresend/internal/transfer"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
)

// drain consumes a node's events, approving any manifest, and returns the
// final result of the transfer.
func drain(t *testing.T, node *p2p.Node) <-chan p2p.DoneEvent {
	t.Helper()
	out := make(chan p2p.DoneEvent, 1)
	go func() {
		for {
			select {
			case <-node.Done():
				return
			case ev := <-node.Events():
				switch e := ev.(type) {
				case p2p.ManifestEvent:
					e.Reply <- true
				case p2p.DoneEvent:
					out <- e
					return
				}
			}
		}
	}()
	return out
}

// TestRoomIsSingleUse is the regression test for the worst behaviour the
// test report turned up: after a transfer completed, the sender kept
// answering the same room code, so anyone else who had been told it could
// download the files again — silently, with the sender's screen still
// saying "sent".
//
// Two things have to hold now. The room is gone from the server, and the
// sender no longer serves the transfer protocol at all. The second matters
// on its own: dropping only the registration would still leave the sender
// answering whoever already knew the code.
func TestRoomIsSingleUse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	_, registry, serverAddr := newWSServer(t)

	sender, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	first, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	srcDir := t.TempDir()
	want := []byte("gizli belge")
	srcPath := filepath.Join(srcDir, "belge.txt")
	if err := os.WriteFile(srcPath, want, 0o644); err != nil {
		t.Fatal(err)
	}

	sendDone := drain(t, sender)
	room, err := sender.Host(ctx, []string{srcPath})
	if err != nil {
		t.Fatal(err)
	}

	// The first receiver — the one the code was meant for — gets the file.
	outDir := t.TempDir()
	firstDone := drain(t, first)
	go first.Fetch(ctx, room, outDir)

	if d := <-firstDone; d.Err != nil {
		t.Fatalf("the intended receiver failed: %v", d.Err)
	}
	if d := <-sendDone; d.Err != nil {
		t.Fatalf("send failed: %v", d.Err)
	}
	if got, _ := os.ReadFile(filepath.Join(outDir, "belge.txt")); !bytes.Equal(got, want) {
		t.Fatal("the intended receiver did not get the file")
	}

	// The server must have forgotten the code, without anyone pressing a
	// key. This was the second half of the report's finding: rooms only
	// ever fell out of the table when their hour was up.
	deadline := time.Now().Add(10 * time.Second)
	for registry.ActiveRooms() > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := registry.ActiveRooms(); got != 0 {
		t.Errorf("%d room(s) still registered after a completed transfer", got)
	}

	// And a second person holding the same code gets nothing, even dialling
	// the sender directly — the code alone must not be enough.
	second, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	secondOut := t.TempDir()
	secondDone := drain(t, second)
	go second.Fetch(ctx, room, secondOut)

	d := <-secondDone
	if d.Err == nil {
		t.Fatal("a second receiver downloaded the files with a used code")
	}
	entries, err := os.ReadDir(secondOut)
	if err == nil {
		for _, e := range entries {
			t.Errorf("the second receiver got something: %s", e.Name())
		}
	}
}

// TestFailedAttemptKeepsTheRoom is the other half of the rule. A transfer
// that breaks — a flaky link, a friend who closed the window — must leave
// the code working, or every hiccup would cost a phone call.
func TestFailedAttemptKeepsTheRoom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	_, registry, serverAddr := newWSServer(t)

	sender, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "belge.txt")
	if err := os.WriteFile(srcPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	sendDone := drain(t, sender)
	room, err := sender.Host(ctx, []string{srcPath})
	if err != nil {
		t.Fatal(err)
	}

	// A receiver that declines: a completed conversation, but not a
	// completed transfer.
	decliner, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer decliner.Close()

	declined := make(chan p2p.DoneEvent, 1)
	go func() {
		for {
			select {
			case <-decliner.Done():
				return
			case ev := <-decliner.Events():
				switch e := ev.(type) {
				case p2p.ManifestEvent:
					e.Reply <- false
				case p2p.DoneEvent:
					declined <- e
					return
				}
			}
		}
	}()
	go decliner.Fetch(ctx, room, t.TempDir())

	if d := <-declined; d.Err == nil {
		t.Fatal("declining reported success")
	}
	if d := <-sendDone; d.Err == nil {
		t.Fatal("the sender did not notice the refusal")
	}

	// The code still works, and a second attempt goes through.
	if got := registry.ActiveRooms(); got != 1 {
		t.Fatalf("a refused transfer retired the code: %d rooms left", got)
	}

	retry, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()

	outDir := t.TempDir()
	retryDone := drain(t, retry)
	sendAgain := drain(t, sender)
	go retry.Fetch(ctx, room, outDir)

	if d := <-retryDone; d.Err != nil {
		t.Fatalf("the retry failed: %v", d.Err)
	}
	if d := <-sendAgain; d.Err != nil {
		t.Fatalf("the retried send failed: %v", d.Err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "belge.txt")); err != nil {
		t.Errorf("the retry saved nothing: %v", err)
	}
}

// TestWrongCodeGetsNothing checks the guarantee the room-code handshake
// adds: reaching the sender is no longer enough, holding the code is. An
// impostor here does everything an honest receiver does — looks the room
// up, dials the sender over the real network — but hands the transfer a
// different code, and gets refused before the file list is even sent.
func TestWrongCodeGetsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	server, _, serverAddr := newWSServer(t)

	sender, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "gizli.txt")
	if err := os.WriteFile(srcPath, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	rejected := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-sender.Done():
				return
			case ev := <-sender.Events():
				switch ev.(type) {
				case p2p.RejectedEvent:
					rejected <- struct{}{}
				case p2p.DoneEvent, p2p.ConnectedEvent:
					t.Errorf("the impostor's attempt reached the sender as %T", ev)
				}
			}
		}
	}()
	room, err := sender.Host(ctx, []string{srcPath})
	if err != nil {
		t.Fatal(err)
	}

	impostor := newHost(t)
	if err := impostor.Connect(ctx, *host.InfoFromHost(server)); err != nil {
		t.Fatal(err)
	}
	info, _, err := rendezvous.Lookup(ctx, impostor, server.ID(), rendezvous.Nameplate(room))
	if err != nil {
		t.Fatal(err)
	}
	if err := impostor.Connect(ctx, *info); err != nil {
		t.Fatal(err)
	}
	s, err := impostor.NewStream(network.WithAllowLimitedConn(ctx, "test"), info.ID, transfer.ProtocolID)
	if err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	sawManifest := false
	_, err = transfer.Receive(s, outDir, transfer.Credentials{
		Code:     "zebra-zebra-99", // not the room code
		Sender:   info.ID.String(),
		Receiver: impostor.ID().String(),
	}, func(transfer.Manifest) bool { sawManifest = true; return true }, transfer.Hooks{})

	if err == nil {
		t.Fatal("an impostor without the code completed the transfer")
	}
	if !strings.Contains(err.Error(), "room code") {
		t.Errorf("unexpected failure for a wrong code: %v", err)
	}
	if sawManifest {
		t.Error("the file list was shown to someone without the code")
	}
	if entries, err := os.ReadDir(outDir); err == nil {
		for _, e := range entries {
			t.Errorf("the impostor got something: %s", e.Name())
		}
	}

	// The sender was told someone tried a wrong code — without its screen
	// leaving "waiting", since no transfer ever started.
	select {
	case <-rejected:
	case <-ctx.Done():
		t.Error("the sender was never told about the wrong code")
	}
}

// TestTooManyWrongCodesCloseTheRoom: the nameplate that finds a room is
// public, so anyone can reach the sender and guess at the words. Each
// guess is a handshake the sender sees, and after MaxWrongCodes of them the
// room is closed — the code stops working, and the sender is told why.
func TestTooManyWrongCodesCloseTheRoom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	server, _, serverAddr := newWSServer(t)
	sender, err := p2p.New(ctx, []string{serverAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	srcPath := filepath.Join(t.TempDir(), "gizli.txt")
	if err := os.WriteFile(srcPath, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	events := collect(sender)
	room, err := sender.Host(ctx, []string{srcPath})
	if err != nil {
		t.Fatal(err)
	}

	guesser := newHost(t)
	if err := guesser.Connect(ctx, *host.InfoFromHost(server)); err != nil {
		t.Fatal(err)
	}
	info, _, err := rendezvous.Lookup(ctx, guesser, server.ID(), rendezvous.Nameplate(room))
	if err != nil {
		t.Fatal(err)
	}
	if err := guesser.Connect(ctx, *info); err != nil {
		t.Fatal(err)
	}
	guess := func(words string) error {
		s, err := guesser.NewStream(network.WithAllowLimitedConn(ctx, "test"), info.ID, transfer.ProtocolID)
		if err != nil {
			return err
		}
		_, err = transfer.Receive(s, t.TempDir(), transfer.Credentials{
			Code:     rendezvous.JoinCode(words, rendezvous.Nameplate(room)),
			Sender:   info.ID.String(),
			Receiver: guesser.ID().String(),
		}, func(transfer.Manifest) bool { return true }, transfer.Hooks{})
		return err
	}

	for want := p2p.MaxWrongCodes - 1; want > 0; want-- {
		if err := guess("zebra-zebra"); !errors.Is(err, transfer.ErrWrongCode) {
			t.Fatalf("a wrong guess got %v", err)
		}
		if got := waitForEvent[p2p.RejectedEvent](t, events, 10*time.Second); got.Left != want {
			t.Errorf("after a wrong guess the sender was told %d are left, want %d", got.Left, want)
		}
	}
	_ = guess("zebra-zebra")
	lost := waitForEvent[p2p.RoomLostEvent](t, events, 10*time.Second)
	if !errors.Is(lost.Err, p2p.ErrTooManyWrongCodes) {
		t.Fatalf("the room was lost with %v, want ErrTooManyWrongCodes", lost.Err)
	}

	// Even the right words are too late now: the room is gone from the
	// server and the sender no longer answers.
	if _, _, err := rendezvous.Lookup(ctx, newHostConnectedTo(t, ctx, server), server.ID(), rendezvous.Nameplate(room)); err == nil {
		t.Error("the closed room can still be looked up")
	}
	if err := guess(strings.TrimSuffix(room, "-"+rendezvous.Nameplate(room))); err == nil {
		t.Error("the right code still works after the room was closed")
	}
}

// newHostConnectedTo is a fresh peer, already connected to the server.
func newHostConnectedTo(t *testing.T, ctx context.Context, server host.Host) host.Host {
	t.Helper()
	h := newHost(t)
	if err := h.Connect(ctx, *host.InfoFromHost(server)); err != nil {
		t.Fatal(err)
	}
	return h
}
