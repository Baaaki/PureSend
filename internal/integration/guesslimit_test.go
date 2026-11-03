package integration

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"puresend/internal/p2p"
	"puresend/internal/rendezvous"
	"puresend/internal/transfer"

	"github.com/libp2p/go-libp2p/core/network"
)

// heldStream lets a handshake run up to the receiver's proof of the code
// and holds it there, the way a guesser would, until released. The
// receiver writes exactly two handshake messages, its half of the exchange
// and then its proof, so the second write is the one held.
type heldStream struct {
	io.ReadWriteCloser
	writes  int
	staged  chan struct{}
	release chan struct{}
}

func (h *heldStream) Write(p []byte) (int, error) {
	h.writes++
	if h.writes == 2 {
		close(h.staged)
		<-h.release
	}
	return h.ReadWriteCloser.Write(p)
}

// TestGuessesHeldOpenTogetherShareTheLimit: a room allows MaxWrongCodes
// guesses, however they are timed. A guesser can hold several handshakes
// open at once, each past the exchange and waiting only on its verdict.
// Wrong codes used to be counted when a handshake ended, so every one held
// open was judged before the count caught up — one more guess than the
// room allows, which here is the right code, and it got the files.
func TestGuessesHeldOpenTogetherShareTheLimit(t *testing.T) {
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

	guesser := newHostConnectedTo(t, ctx, server)
	info, _, err := rendezvous.Lookup(ctx, guesser, server.ID(), rendezvous.Nameplate(room))
	if err != nil {
		t.Fatal(err)
	}
	if err := guesser.Connect(ctx, *info); err != nil {
		t.Fatal(err)
	}

	// Every wrong guess the room allows, and one more: the right code.
	nameplate := rendezvous.Nameplate(room)
	var codes []string
	for _, words := range []string{"zebra-zebra", "zebra-kiraz", "kiraz-zebra"}[:p2p.MaxWrongCodes] {
		codes = append(codes, rendezvous.JoinCode(words, nameplate))
	}
	codes = append(codes, room)

	type attempt struct {
		held        *heldStream
		result      chan error
		sawManifest bool
	}
	attempts := make([]*attempt, len(codes))
	for i, code := range codes {
		s, err := guesser.NewStream(network.WithAllowLimitedConn(ctx, "test"), info.ID, transfer.ProtocolID)
		if err != nil {
			t.Fatal(err)
		}
		a := &attempt{
			held:   &heldStream{ReadWriteCloser: s, staged: make(chan struct{}), release: make(chan struct{})},
			result: make(chan error, 1),
		}
		attempts[i] = a
		go func() {
			_, err := transfer.Receive(a.held, t.TempDir(), transfer.Credentials{
				Code:     code,
				Sender:   info.ID.String(),
				Receiver: guesser.ID().String(),
			}, func(transfer.Manifest) bool { a.sawManifest = true; return false }, transfer.Hooks{})
			a.result <- err
		}()
	}
	for _, a := range attempts {
		select {
		case <-a.held.staged:
		case <-ctx.Done():
			t.Fatal("a handshake never got as far as its proof")
		}
	}

	// The wrong guesses first, one verdict at a time; the right one is
	// still held open, its exchange done, when the last of them closes
	// the room.
	for i, a := range attempts {
		close(a.held.release)
		err := <-a.result
		if i < p2p.MaxWrongCodes {
			if !errors.Is(err, transfer.ErrWrongCode) {
				t.Fatalf("wrong guess %d got %v", i+1, err)
			}
			continue
		}
		if !errors.Is(err, transfer.ErrWrongCode) || a.sawManifest {
			t.Fatalf("a guess past the limit was judged: it ended with %v (file list shown: %v), "+
				"want it refused as a wrong code", err, a.sawManifest)
		}
	}

	lost := waitForEvent[p2p.RoomLostEvent](t, events, 10*time.Second)
	if !errors.Is(lost.Err, p2p.ErrTooManyWrongCodes) {
		t.Fatalf("the room was lost with %v, want ErrTooManyWrongCodes", lost.Err)
	}
}
