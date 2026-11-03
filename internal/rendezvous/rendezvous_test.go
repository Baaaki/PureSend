package rendezvous

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

// The Registry doubles as the relay service's access control filter.
var _ relay.ACLFilter = (*Registry)(nil)

var (
	sender   = peer.ID("sender")
	receiver = peer.ID("receiver")
)

func register(r *Registry, from peer.ID, nameplate string) Response {
	return r.handle(from, Request{
		Type:      "register",
		Nameplate: nameplate,
		Addrs:     []string{"/ip4/127.0.0.1/tcp/4001"},
	})
}

// open registers a new room for from and returns its nameplate.
func open(t *testing.T, r *Registry, from peer.ID) string {
	t.Helper()
	resp := register(r, from, "")
	if resp.Type != "ok" {
		t.Fatalf("register failed: %+v", resp)
	}
	return resp.Nameplate
}

func lookup(r *Registry, from peer.ID, nameplate string) Response {
	return r.handle(from, Request{Type: "lookup", Nameplate: nameplate})
}

// unused returns a well-formed nameplate nobody holds.
func unused(r *Registry) string {
	for n := 9999; ; n-- {
		if _, taken := r.rooms[strconv.Itoa(n)]; !taken {
			return strconv.Itoa(n)
		}
	}
}

func TestRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)
	if !ValidNameplate(np) {
		t.Fatalf("the server handed out %q", np)
	}

	resp := lookup(r, receiver, np)
	if resp.Type != "found" {
		t.Fatalf("lookup failed: %+v", resp)
	}
	if resp.PeerID != sender.String() {
		t.Errorf("lookup returned peer %q, want %q", resp.PeerID, sender.String())
	}
	if len(resp.Addrs) != 1 {
		t.Errorf("lookup returned %d addrs, want 1", len(resp.Addrs))
	}
}

// TestNameplatesStayShortAndSparse: two digits while few rooms are open,
// longer ones once the short range would get crowded — crowded meaning a
// mistyped number would too often land in someone else's room.
func TestNameplatesStayShortAndSparse(t *testing.T) {
	r := NewRegistry()
	seen := map[string]bool{}
	for i := range 200 {
		np := open(t, r, peer.ID(fmt.Sprintf("p%d", i)))
		if seen[np] {
			t.Fatalf("nameplate %s handed out twice", np)
		}
		seen[np] = true
	}
	lengths := map[int]int{}
	for np := range seen {
		lengths[len(np)]++
	}
	if lengths[2] == 0 || lengths[2] > 90/nameplateSparsity {
		t.Errorf("%d two-digit nameplates among 200 rooms, want 1..%d", lengths[2], 90/nameplateSparsity)
	}
	if lengths[3] == 0 || lengths[3] > 900/nameplateSparsity {
		t.Errorf("%d three-digit nameplates among 200 rooms, want 1..%d", lengths[3], 900/nameplateSparsity)
	}
	if lengths[4] == 0 {
		t.Error("no four-digit nameplates once the shorter ranges were crowded")
	}
}

// TestNameplatesScaleDynamically verifies that when the table grows beyond
// 4-digit capacity, nameplates scale dynamically to 5 digits and beyond
// without panic or arbitrary limit.
func TestNameplatesScaleDynamically(t *testing.T) {
	r := NewRegistry(WithMaxRooms(0))
	for i := 10; i < 19; i++ {
		r.rooms[strconv.Itoa(i)] = roomEntry{}
	}
	for i := 100; i < 190; i++ {
		r.rooms[strconv.Itoa(i)] = roomEntry{}
	}
	for i := 1000; i < 1900; i++ {
		r.rooms[strconv.Itoa(i)] = roomEntry{}
	}
	np := r.freeNameplate()
	if len(np) != 5 {
		t.Fatalf("expected 5-digit nameplate when shorter ranges are crowded, got %s (len %d)", np, len(np))
	}
	if !ValidNameplate(np) {
		t.Fatalf("5-digit nameplate %s is not valid according to ValidNameplate", np)
	}
}

func TestLookupUnknownRoom(t *testing.T) {
	r := NewRegistry()
	if resp := lookup(r, receiver, "11"); resp.Type != "not_found" {
		t.Fatalf("got %+v, want not_found", resp)
	}
}

func TestRegisterValidation(t *testing.T) {
	r := NewRegistry()
	if resp := r.handle(sender, Request{Type: "register"}); resp.Type != "error" {
		t.Errorf("register without addresses accepted: %+v", resp)
	}
	if resp := r.handle(sender, Request{Type: "bogus"}); resp.Type != "error" {
		t.Errorf("unknown request type accepted: %+v", resp)
	}

	tooMany := make([]string, MaxAddrs+1)
	for i := range tooMany {
		tooMany[i] = "/ip4/127.0.0.1/tcp/4001"
	}
	if resp := r.handle(sender, Request{Type: "register", Addrs: tooMany}); resp.Type != "error" {
		t.Errorf("register with too many addresses accepted: %+v", resp)
	}
	if resp := r.handle(sender, Request{Type: "register", Addrs: []string{"garbage"}}); resp.Type != "error" {
		t.Errorf("register without a single valid address accepted: %+v", resp)
	}
	if r.ActiveRooms() != 0 {
		t.Errorf("a refused register left %d rooms behind", r.ActiveRooms())
	}
}

// TestRegisterRejectsMalformedNameplates: a room name used to be up to 16 KB
// of anything at all. Now it is a number of at least two digits — and in
// particular never a whole room code, whose words the server must not see.
func TestRegisterRejectsMalformedNameplates(t *testing.T) {
	for _, bad := range []string{
		"kiraz-liman-42",
		"7",
		"042",
		"4a",
		"42\n",
		" 42",
		strings.Repeat("9", 16<<10),
	} {
		r := NewRegistry()
		if resp := register(r, sender, bad); resp.Type != "error" {
			t.Errorf("register accepted the nameplate %q", bad)
		}
		if resp := lookup(r, receiver, bad); resp.Type != "error" {
			t.Errorf("lookup accepted the nameplate %q", bad)
		}
		if r.ActiveRooms() != 0 {
			t.Errorf("a room was created for %q", bad)
		}
	}
}

func TestRoomTakeoverRejected(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)

	if resp := register(r, peer.ID("attacker"), np); resp.Type != "error" || resp.Error != msgInUse {
		t.Fatalf("takeover by another peer got %+v", resp)
	}
	// The owner itself may refresh its own room, and keeps its number.
	if resp := register(r, sender, np); resp.Type != "ok" || resp.Nameplate != np {
		t.Fatalf("owner re-register got %+v", resp)
	}
}

// TestNameplateTakenBack: a server that restarts forgets every room. The
// sender registers its nameplate again, and the code its user has already
// read out keeps working.
func TestNameplateTakenBack(t *testing.T) {
	r := NewRegistry()
	if resp := register(r, sender, "427"); resp.Type != "ok" || resp.Nameplate != "427" {
		t.Fatalf("taking back a free nameplate got %+v", resp)
	}
	if resp := lookup(r, receiver, "427"); resp.Type != "found" {
		t.Fatalf("the room taken back is not found: %+v", resp)
	}
}

func TestRoomExpiry(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)

	// Age the entry past its TTL by editing it directly.
	e := r.rooms[np]
	e.createdAt = time.Now().Add(-RoomTTL - time.Minute)
	r.rooms[np] = e

	if resp := lookup(r, receiver, np); resp.Type != "not_found" {
		t.Fatalf("expired room still found: %+v", resp)
	}
	if got := r.Stats().Expired; got != 1 {
		t.Errorf("Expired = %d, want 1", got)
	}
}

// TestRefreshKeepsTheClock: re-registering — which the client does after
// every reconnect — must not stretch a code past its hour.
func TestRefreshKeepsTheClock(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)
	e := r.rooms[np]
	e.createdAt = time.Now().Add(-RoomTTL + time.Second)
	r.rooms[np] = e

	if resp := register(r, sender, np); resp.Type != "ok" {
		t.Fatalf("refresh failed: %+v", resp)
	}
	time.Sleep(1100 * time.Millisecond)
	if resp := lookup(r, receiver, np); resp.Type != "not_found" {
		t.Fatalf("a refresh extended the room past its hour: %+v", resp)
	}
}

func TestServerFull(t *testing.T) {
	r := NewRegistry()
	for i := range MaxRooms {
		if resp := register(r, peer.ID(fmt.Sprintf("p%d", i)), ""); resp.Type != "ok" {
			t.Fatalf("register %d failed: %+v", i, resp)
		}
	}
	if resp := register(r, sender, ""); resp.Type != "error" || !strings.Contains(resp.Error, "full") {
		t.Fatalf("register beyond MaxRooms got %+v", resp)
	}
}

// TestFullTableMakesRoomFromAbandonedRooms: registering and hanging up in
// a loop leaves rooms nobody can reach. They must not keep a sender who is
// actually here out of a full table.
func TestFullTableMakesRoomFromAbandonedRooms(t *testing.T) {
	r := NewRegistry()
	for i := range MaxRooms {
		open(t, r, peer.ID(fmt.Sprintf("p%d", i)))
	}
	r.ownerGone(peer.ID("p7"))

	np := open(t, r, peer.ID("latecomer"))
	if resp := lookup(r, receiver, np); resp.Type != "found" {
		t.Fatalf("the latecomer's room is not there: %+v", resp)
	}
	if got := r.Stats().Evicted; got != 1 {
		t.Errorf("Evicted = %d, want 1", got)
	}
	if r.HasPeer(peer.ID("p7")) {
		t.Error("the abandoned room was kept instead of the latecomer's")
	}
	if got := r.ActiveRooms(); got != MaxRooms {
		t.Errorf("ActiveRooms = %d, want %d", got, MaxRooms)
	}

	// With every owner still connected there is nothing to give up.
	if resp := register(r, peer.ID("one-too-many"), ""); resp.Type != "error" {
		t.Errorf("a full table with no abandoned rooms took another: %+v", resp)
	}
}

// TestNoRegisterKillSwitch is the finding that removed the server-wide
// budget of new rooms per minute. Identities cost nothing, so spending the
// budget with fresh ones told every real sender "server is busy". As long
// as the table has space, a new room is never refused for being one too
// many this minute.
func TestNoRegisterKillSwitch(t *testing.T) {
	r := NewRegistry()
	for i := range 500 {
		if resp := register(r, peer.ID(fmt.Sprintf("fresh-identity-%d", i)), ""); resp.Type != "ok" {
			t.Fatalf("register %d within the minute got %+v", i, resp)
		}
	}
	if resp := register(r, peer.ID("the-real-sender"), ""); resp.Type != "ok" {
		t.Fatalf("after a burst of registrations a real sender got %+v", resp)
	}
}

// TestRoomsPerPeerLimit checks that one sender cannot claim the whole
// table and leave every other user with "server is full".
func TestRoomsPerPeerLimit(t *testing.T) {
	r := NewRegistry(WithMaxRoomsPerPeer(2))
	first := open(t, r, sender)
	open(t, r, sender)
	if resp := register(r, sender, ""); resp.Type != "error" {
		t.Fatalf("register beyond the per-peer limit accepted: %+v", resp)
	}
	// Another sender is unaffected, and so is refreshing a room already held.
	open(t, r, receiver)
	if resp := register(r, sender, first); resp.Type != "ok" {
		t.Errorf("refreshing an owned room was refused: %+v", resp)
	}
	// Closing one frees the slot again.
	if resp := r.handle(sender, Request{Type: "unregister", Nameplate: first}); resp.Type != "ok" {
		t.Fatalf("unregister failed: %+v", resp)
	}
	if resp := register(r, sender, ""); resp.Type != "ok" {
		t.Errorf("a freed slot was not reusable: %+v", resp)
	}
}

// TestOneRoomPerPeerByDefault: the program opens one room per session, so
// that is all a peer gets unless the operator says otherwise.
func TestOneRoomPerPeerByDefault(t *testing.T) {
	r := NewRegistry()
	open(t, r, sender)
	if resp := register(r, sender, ""); resp.Type != "error" {
		t.Errorf("a second room for the same peer was accepted: %+v", resp)
	}
}

// TestOwnerGone: a room is only reachable while its owner is connected to
// the server. One whose owner has vanished is kept for a short grace
// period — long enough to reconnect — and then dropped, instead of taking
// a slot for the rest of its hour.
func TestOwnerGone(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)

	r.ownerGone(sender)
	resp := lookup(r, receiver, np)
	if resp.Type != "error" || !strings.Contains(resp.Error, "reconnecting") {
		t.Fatalf("lookup while the owner is away got %+v", resp)
	}

	r.ownerBack(sender)
	if resp := lookup(r, receiver, np); resp.Type != "found" {
		t.Fatalf("the room did not come back with its owner: %+v", resp)
	}

	r.ownerGone(sender)
	e := r.rooms[np]
	e.ownerGone = time.Now().Add(-ownerGrace - time.Second)
	r.rooms[np] = e
	if got := r.ActiveRooms(); got != 0 {
		t.Fatalf("an abandoned room outlived its grace period: %d rooms", got)
	}
	if got := r.Stats().Abandoned; got != 1 {
		t.Errorf("Abandoned = %d, want 1", got)
	}
	// And the owner, back later, can simply take its nameplate back.
	if resp := register(r, sender, np); resp.Type != "ok" || resp.Nameplate != np {
		t.Errorf("re-registering after a long absence got %+v", resp)
	}
}

// TestUnregister checks that a finished sender can retire its code
// immediately, and that nobody else can retire it for them.
func TestUnregister(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)

	if resp := r.handle(peer.ID("attacker"), Request{Type: "unregister", Nameplate: np}); resp.Type != "ok" {
		t.Fatalf("unregister by a stranger returned %+v, want a plain ok", resp)
	}
	if resp := lookup(r, receiver, np); resp.Type != "found" {
		t.Fatal("a stranger managed to close someone else's room")
	}

	if resp := r.handle(sender, Request{Type: "unregister", Nameplate: np}); resp.Type != "ok" {
		t.Fatalf("owner unregister failed: %+v", resp)
	}
	if resp := lookup(r, receiver, np); resp.Type != "not_found" {
		t.Fatalf("the room survived its owner closing it: %+v", resp)
	}
	if got := r.ActiveRooms(); got != 0 {
		t.Errorf("ActiveRooms is %d after the only room closed, want 0", got)
	}
}

// TestLookupReportsRelayLimit checks that a receiver is told how much the
// server will relay, which is what lets it warn before a transfer that
// cannot fit through the fallback route.
func TestLookupReportsRelayLimit(t *testing.T) {
	r := NewRegistry(WithRelayLimit(256 << 20))
	np := open(t, r, sender)

	resp := lookup(r, receiver, np)
	if resp.RelayLimit != 256<<20 {
		t.Errorf("lookup reported a relay limit of %d, want %d", resp.RelayLimit, 256<<20)
	}
}

// TestStats checks the counters the health endpoint and the metrics
// exporter are built on.
func TestStats(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)
	lookup(r, receiver, np)
	lookup(r, receiver, unused(r))
	r.handle(sender, Request{Type: "unregister", Nameplate: np})

	got := r.Stats()
	want := Stats{Registered: 1, Unregistered: 1, LookupsFound: 1, LookupsNotFound: 1}
	if got != want {
		t.Errorf("Stats() = %+v, want %+v", got, want)
	}
}

func TestRelayACL(t *testing.T) {
	r := NewRegistry()

	if r.AllowReserve(sender, nil) {
		t.Error("reservation allowed for a peer without a room")
	}
	open(t, r, sender)
	if !r.AllowReserve(sender, nil) {
		t.Error("reservation refused for a room owner")
	}
	if !r.AllowConnect(receiver, nil, sender) {
		t.Error("relayed connection towards a room owner refused")
	}
	if r.AllowConnect(sender, nil, receiver) {
		t.Error("relayed connection allowed towards a peer without a room")
	}
}

// TestLookupRateLimit: nameplates are public, so what is limited is how
// fast one peer can walk them — rooms found count as much as misses.
func TestLookupRateLimit(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)

	for i := range maxLookups {
		target, want := np, "found"
		if i%2 == 1 {
			target, want = unused(r), "not_found"
		}
		if resp := lookup(r, receiver, target); resp.Type != want {
			t.Fatalf("lookup %d got %+v, want %s", i, resp, want)
		}
	}
	if resp := lookup(r, receiver, np); resp.Type != "error" {
		t.Fatalf("rate limit not applied: %+v", resp)
	}
	if got := r.Stats().LookupsThrottled; got != 1 {
		t.Errorf("LookupsThrottled = %d, want 1", got)
	}
	// Other peers are unaffected.
	if resp := lookup(r, peer.ID("other"), np); resp.Type != "found" {
		t.Fatalf("rate limit leaked to another peer: %+v", resp)
	}
	// The block lifts once the window expires.
	r.lookups[receiver] = window{count: maxLookups, start: time.Now().Add(-2 * lookupWindow)}
	if resp := lookup(r, receiver, np); resp.Type != "found" {
		t.Fatalf("rate limit did not expire: %+v", resp)
	}
}

// TestMalformedLookupIsNotCounted: a nameplate that cannot exist teaches
// nothing, and a typo in its shape should not use up one of a user's tries.
func TestMalformedLookupIsNotCounted(t *testing.T) {
	r := NewRegistry()
	for range maxLookups + 2 {
		lookup(r, receiver, "not a number")
	}
	np := open(t, r, sender)
	if resp := lookup(r, receiver, np); resp.Type != "found" {
		t.Fatalf("malformed lookups counted against the peer: %+v", resp)
	}
}

// fillGlobalBudget spends the server-wide miss budget with fresh
// identities — the walker who reconnects between lookups.
func fillGlobalBudget(t *testing.T, r *Registry) {
	t.Helper()
	for i := range maxGlobalMisses {
		walker := peer.ID(fmt.Sprintf("fresh-identity-%d", i))
		if resp := lookup(r, walker, unused(r)); resp.Type != "not_found" {
			t.Fatalf("miss %d not reported: %+v", i, resp)
		}
	}
}

// TestGlobalLimitIsNotAKillSwitch is the finding that reshaped this
// limit. It used to refuse every lookup once the budget was spent, so a
// few random requests a second closed the door on every real user. Someone
// typing the code they were given must still get through.
func TestGlobalLimitIsNotAKillSwitch(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)
	fillGlobalBudget(t, r)

	if resp := lookup(r, peer.ID("the-real-friend"), np); resp.Type != "found" {
		t.Fatalf("under a flood of lookups, a first lookup was refused: %+v", resp)
	}
}

// TestGlobalLimitTakesAwaySecondChances covers what the limit still does
// under pressure: a peer gets one lookup, answered honestly, and nothing
// after it.
func TestGlobalLimitTakesAwaySecondChances(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)
	fillGlobalBudget(t, r)

	walker := peer.ID("walker")
	if resp := lookup(r, walker, unused(r)); resp.Type != "not_found" {
		t.Fatalf("a first lookup under pressure got %+v, want an honest not_found", resp)
	}
	if resp := lookup(r, walker, np); resp.Type != "error" {
		t.Fatalf("a second lookup under pressure was answered: %+v", resp)
	}

	// The pressure lifts with the window.
	r.globalMiss = window{count: maxGlobalMisses, start: time.Now().Add(-2 * lookupWindow)}
	if resp := lookup(r, walker, np); resp.Type != "found" {
		t.Fatalf("the server-wide limit did not expire: %+v", resp)
	}
}

func TestUnregisterAndReRegisterPreservesCreatedAt(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)

	original := time.Now().Add(-45 * time.Minute)
	e := r.rooms[np]
	e.createdAt = original
	r.rooms[np] = e

	if resp := r.handle(sender, Request{Type: "unregister", Nameplate: np}); resp.Type != "ok" {
		t.Fatalf("unregister failed: %+v", resp)
	}
	if resp := register(r, sender, np); resp.Type != "ok" {
		t.Fatalf("re-register failed: %+v", resp)
	}
	if got := r.rooms[np].createdAt; !got.Equal(original) {
		t.Errorf("createdAt after Unregister+Register = %v, want original %v", got, original)
	}
}

func TestLookupsMapCapAndHardMissLimit(t *testing.T) {
	r := NewRegistry()
	np := open(t, r, sender)

	// Hard global miss cap stops fresh-identity walking floods completely.
	for i := range maxGlobalMissesHard {
		id := peer.ID(fmt.Sprintf("bot-%d", i))
		if resp := lookup(r, id, unused(r)); resp.Type != "not_found" {
			t.Fatalf("miss %d got %+v, want not_found", i, resp)
		}
	}
	if resp := lookup(r, peer.ID("bot-over-hard-cap"), np); resp.Type != "error" {
		t.Fatalf("lookup past hard global miss cap got %+v, want error", resp)
	}

	// Reset globalMiss and verify maxLookupPeers cap bounds the map.
	r2 := NewRegistry()
	now := time.Now()
	for i := range maxLookupPeers {
		r2.lookups[peer.ID(fmt.Sprintf("peer-%d", i))] = window{count: 1, start: now}
	}
	if resp := lookup(r2, peer.ID("overflow-peer"), "42"); resp.Type != "error" {
		t.Fatalf("lookup when lookups map is full got %+v, want error", resp)
	}
	if len(r2.lookups) > maxLookupPeers {
		t.Errorf("len(lookups) = %d, exceeds maxLookupPeers (%d)", len(r2.lookups), maxLookupPeers)
	}
}
