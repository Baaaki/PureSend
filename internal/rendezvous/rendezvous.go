// Package rendezvous implements a simple "room" protocol that lets two
// peers find each other through a shared server.
//
// The flow:
//  1. The sender connects to the server and registers its addresses. The
//     server hands back a nameplate, the number at the end of the room
//     code; the sender puts its own secret words in front of it.
//  2. The receiver asks the server for that nameplate and gets the
//     sender's addresses back.
//  3. From that point on the server is out of the picture; the receiver
//     connects to the sender directly, and the two prove to each other
//     that they hold the whole code.
//  4. When the transfer is over the sender drops the room, so the code
//     stops working immediately instead of lingering until it expires.
//
// The protocol exchanges single-line JSON messages: the client sends one
// Request, the server replies with one Response, and the stream closes.
//
// The server is not a trusted party. It learns which peers are talking and
// when, but it never sees the secret words of a code, so it cannot read a
// transfer, and it cannot put a peer of its own in the sender's place — or
// in the receiver's — without guessing them. See code.go.
package rendezvous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"strconv"
	"sync"
	"time"

	"puresend/internal/safetext"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// ProtocolID identifies the rendezvous protocol on libp2p.
//
//	1.1.0 added "unregister" and the relay limit in a lookup response.
//	2.0.0 finds rooms by a nameplate the server hands out, instead of by
//	      the whole room code, so the server never learns the secret half.
const ProtocolID = "/puresend/rendezvous/2.0.0"

// RoomTTL is how long a code lasts at most. The sender normally drops its
// room as soon as the transfer finishes; this is the backstop for the
// sender that was closed or crashed. Refreshing a room does not extend
// it, so "a code lasts at most an hour" holds whatever a client does.
const RoomTTL = 1 * time.Hour

// MaxRooms is how many rooms the server keeps at once. Exported so the
// server can size its relay to match: every room owner holds one relay
// reservation.
const MaxRooms = 1000

// DefaultMaxRoomsPerPeer bounds how many rooms one peer may hold. The
// program only ever opens one per session, so one is all an honest client
// needs — and every extra room a peer may hold is a room an attacker gets
// for free when filling the table.
const DefaultMaxRoomsPerPeer = 1

// ownerGrace is how long a room outlives its owner's connection to the
// server. A room is only reachable while its owner is connected — the
// relay needs that connection — so a room whose owner has gone is kept
// just long enough for a reconnect to pick it up again, instead of taking
// a slot in the table for the rest of its hour.
const ownerGrace = 1 * time.Minute

// Nameplates are public, so a lookup no longer tests a guess at anything:
// it says where a room is, which the nameplate never hid. What is worth
// slowing down is walking them. Every room found is one a stranger can
// knock on, and since a sender closes its room after a few wrong codes, a
// stranger who knocks on every room can close them all. Two limits:
//
//   - Per peer: maxLookups lookups, found or not, within lookupWindow.
//   - Server-wide: once maxGlobalMisses lookups for nameplates nobody holds
//     have piled up within the window, the server is "under pressure", and
//     during that time a peer that has already looked once gets nothing
//     more. Walking a sparse range is mostly misses, so that is what gives
//     a walk away.
//
// Both are soft: a client mints a fresh identity on every start. Rate
// limiting by IP would be the obvious fix, but this server runs behind a
// tunnel, where every client arrives from the same address — an IP limit
// is the tunnel's job (see docs/DEPLOYMENT.md), not this server's. What the
// limits here do is make every few lookups cost a new connection, which is
// what the tunnel counts.
//
// Neither may become a switch that shuts real users out. An earlier
// version refused *everyone* once a server-wide budget was spent, and a
// few random requests a second then closed the door on every real user.
// Someone typing the code they were given gets their first lookup
// answered, however much else is going on.
const (
	lookupWindow    = 1 * time.Minute
	maxLookups      = 5
	maxGlobalMisses = 200
)

// Bounds on a single protocol message, so a malicious client cannot
// make the other side buffer unlimited data.
const maxMessageBytes = 16 << 10 // one JSON request/response

// MaxAddrs bounds the address list in a register request. Exported so
// the client can trim its own list before registering: a node with
// several interfaces and transports easily advertises dozens of
// addresses.
const MaxAddrs = 64

// maxServerText bounds a message from the server before it is shown.
const maxServerText = 200

// Messages the client matches on to tell the user something specific.
const (
	msgInUse        = "room code is already in use"
	msgReconnecting = "the sender is reconnecting, try again in a moment"
)

// Request is the message sent from client to server.
type Request struct {
	Type string `json:"type"` // "register", "lookup" or "unregister"

	// Nameplate names the room, e.g. "42" for the code "kiraz-liman-42".
	// A register without one asks the server for a new room; with one, it
	// refreshes a room the sender holds, or takes that nameplate back after
	// the server has forgotten it.
	Nameplate string `json:"nameplate,omitempty"`

	Addrs []string `json:"addrs,omitempty"` // register: the sender's multiaddrs
}

// Response is the server's reply.
type Response struct {
	Type      string   `json:"type"`                // "ok", "found", "not_found" or "error"
	Nameplate string   `json:"nameplate,omitempty"` // register: the room's nameplate
	PeerID    string   `json:"peer_id,omitempty"`
	Addrs     []string `json:"addrs,omitempty"`
	Error     string   `json:"error,omitempty"`

	// RelayLimit is how many bytes this server will relay for a single
	// connection, sent with a lookup so the receiver can warn before
	// starting a transfer that cannot possibly fit through the fallback
	// route. Zero means the server did not say.
	RelayLimit int64 `json:"relay_limit,omitempty"`
}

// ---------------------------------------------------------------------------
// Client side
// ---------------------------------------------------------------------------

// ErrInUse means someone else holds the nameplate a sender asked to have
// back — after the server forgot its room, say, and handed the number out
// again. The code built on it is gone.
var ErrInUse = fmt.Errorf("server rejected registration: %s", msgInUse)

// Register stores the given addresses on the server and returns the room's
// nameplate. The sending side calls this so the receiver can find it: with
// an empty nameplate for a new room, and with the one it holds to refresh
// it after reconnecting.
func Register(ctx context.Context, h host.Host, server peer.ID, nameplate string, addrs []multiaddr.Multiaddr) (string, error) {
	addrStrs := make([]string, len(addrs))
	for i, a := range addrs {
		addrStrs[i] = a.String()
	}

	resp, err := roundTrip(ctx, h, server, Request{Type: "register", Nameplate: nameplate, Addrs: addrStrs})
	if err != nil {
		return "", err
	}
	if resp.Type != "ok" {
		if resp.Error == msgInUse {
			return "", ErrInUse
		}
		return "", fmt.Errorf("server rejected registration: %s", safetext.Clean(resp.Error, maxServerText))
	}
	// The nameplate becomes part of the code on the sender's screen, so it
	// is held to its shape here rather than trusted.
	if !ValidNameplate(resp.Nameplate) || (nameplate != "" && resp.Nameplate != nameplate) {
		return "", fmt.Errorf("server gave an invalid room number")
	}
	return resp.Nameplate, nil
}

// Unregister drops a room the sender owns. Called as soon as a transfer
// finishes, so the code stops working the moment it has done its job
// rather than an hour later.
func Unregister(ctx context.Context, h host.Host, server peer.ID, nameplate string) error {
	resp, err := roundTrip(ctx, h, server, Request{Type: "unregister", Nameplate: nameplate})
	if err != nil {
		return err
	}
	if resp.Type != "ok" {
		return fmt.Errorf("server rejected the request to close the room: %s", safetext.Clean(resp.Error, maxServerText))
	}
	return nil
}

// ErrRoomNotFound is returned when a requested room does not exist on the server.
var ErrRoomNotFound = errors.New("room not found — the code may be wrong or expired")

// Lookup asks the server for a nameplate and returns the registered peer's
// connection info. The receiving side calls this. The server's answer is
// not trusted: the transfer handshake is what proves the peer is the one
// holding the code.
func Lookup(ctx context.Context, h host.Host, server peer.ID, nameplate string) (*peer.AddrInfo, int64, error) {
	resp, err := roundTrip(ctx, h, server, Request{Type: "lookup", Nameplate: nameplate})
	if err != nil {
		return nil, 0, err
	}
	switch resp.Type {
	case "found":
		id, err := peer.Decode(resp.PeerID)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid peer ID from server: %w", err)
		}
		info := &peer.AddrInfo{ID: id}
		for _, s := range resp.Addrs {
			a, err := multiaddr.NewMultiaddr(s)
			if err != nil {
				continue // skip a malformed address; the rest may suffice
			}
			info.Addrs = append(info.Addrs, a)
		}
		if len(info.Addrs) == 0 {
			return nil, 0, fmt.Errorf("no valid addresses registered in the room")
		}
		return info, resp.RelayLimit, nil
	case "not_found":
		return nil, 0, fmt.Errorf("room %s: %w", nameplate, ErrRoomNotFound)
	default:
		return nil, 0, fmt.Errorf("server error: %s", safetext.Clean(resp.Error, maxServerText))
	}
}

// roundTrip sends one request to the server and reads one response.
func roundTrip(ctx context.Context, h host.Host, server peer.ID, req Request) (*Response, error) {
	s, err := h.NewStream(ctx, server, ProtocolID)
	if err != nil {
		return nil, fmt.Errorf("could not open stream to server: %w", err)
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(30 * time.Second))

	if err := json.NewEncoder(s).Encode(req); err != nil {
		return nil, fmt.Errorf("could not send request: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(io.LimitReader(s, maxMessageBytes)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("could not read server response: %w", err)
	}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// Server side
// ---------------------------------------------------------------------------

type roomEntry struct {
	info      peer.AddrInfo
	createdAt time.Time

	// ownerGone is when the owner's last connection to the server closed;
	// zero while it is connected.
	ownerGone time.Time
}

// window counts events inside a fixed time window.
type window struct {
	count int
	start time.Time
}

// current returns the window as of now, starting a fresh one if the old
// one has run out.
func (w window) current(now time.Time, length time.Duration) window {
	if now.Sub(w.start) > length {
		return window{start: now}
	}
	return w
}

// Stats is a snapshot of what the server has been doing, for the health
// endpoint and the metrics exporter.
type Stats struct {
	ActiveRooms      int
	Registered       uint64 // rooms opened since start
	Unregistered     uint64 // rooms closed by their owner
	Expired          uint64 // rooms dropped at the end of their hour
	Abandoned        uint64 // rooms dropped because their owner disconnected
	Evicted          uint64 // abandoned rooms dropped early to make space
	LookupsFound     uint64
	LookupsNotFound  uint64
	LookupsThrottled uint64
	Rejected         uint64 // requests refused for any other reason
}

// Registry is a simple in-memory ledger of active rooms, by nameplate.
type Registry struct {
	maxPerPeer int
	maxRooms   int // 0 means unlimited
	relayLimit int64

	mu         sync.Mutex
	rooms      map[string]roomEntry
	lookups    map[peer.ID]window
	globalMiss window
	stats      Stats
}

// Option configures a Registry.
type Option func(*Registry)

// WithMaxRoomsPerPeer overrides how many rooms one peer may hold at once.
func WithMaxRoomsPerPeer(n int) Option {
	return func(r *Registry) {
		if n > 0 {
			r.maxPerPeer = n
		}
	}
}

// WithMaxRooms overrides the maximum concurrent rooms allowed on the server.
// 0 or a negative value means unlimited.
func WithMaxRooms(n int) Option {
	return func(r *Registry) {
		if n < 0 {
			r.maxRooms = 0
		} else {
			r.maxRooms = n
		}
	}
}

// WithRelayLimit tells the registry how many bytes the relay service will
// carry per connection, so it can pass that on to receivers.
func WithRelayLimit(n int64) Option {
	return func(r *Registry) { r.relayLimit = n }
}

func NewRegistry(opts ...Option) *Registry {
	r := &Registry{
		maxPerPeer: DefaultMaxRoomsPerPeer,
		maxRooms:   MaxRooms,
		rooms:      make(map[string]roomEntry),
		lookups:    make(map[peer.ID]window),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Serve puts the registry to work on a host: it answers the rendezvous
// protocol, and it watches connections come and go, so a room whose owner
// has disconnected stops taking up space.
func (r *Registry) Serve(h host.Host) {
	h.SetStreamHandler(ProtocolID, r.Handler)
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, c network.Conn) {
			r.ownerBack(c.RemotePeer())
		},
		DisconnectedF: func(n network.Network, c network.Conn) {
			if n.Connectedness(c.RemotePeer()) != network.Connected {
				r.ownerGone(c.RemotePeer())
			}
		},
	})
}

// Handler is registered as the server's stream handler.
func (r *Registry) Handler(s network.Stream) {
	defer s.Close()
	s.SetDeadline(time.Now().Add(30 * time.Second))

	var req Request
	if err := json.NewDecoder(io.LimitReader(s, maxMessageBytes)).Decode(&req); err != nil {
		return
	}

	resp := r.handle(s.Conn().RemotePeer(), req)
	json.NewEncoder(s).Encode(resp)
}

func (r *Registry) handle(from peer.ID, req Request) Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropExpired()

	switch req.Type {
	case "register":
		return r.register(from, req)
	case "unregister":
		return r.unregister(from, req)
	case "lookup":
		return r.lookup(from, req)
	default:
		r.stats.Rejected++
		return Response{Type: "error", Error: "unknown request type"}
	}
}

// register is called with the lock held.
func (r *Registry) register(from peer.ID, req Request) Response {
	reject := func(msg string) Response {
		r.stats.Rejected++
		return Response{Type: "error", Error: msg}
	}

	// Nameplates have one shape, and the server holds every client to it:
	// otherwise a room name is up to 16 KB of whatever anyone likes.
	if req.Nameplate != "" && !ValidNameplate(req.Nameplate) {
		return reject("malformed room number")
	}
	if len(req.Addrs) == 0 {
		return reject("an address list is required")
	}
	if len(req.Addrs) > MaxAddrs {
		return reject("too many addresses")
	}
	info := peer.AddrInfo{ID: from}
	for _, s := range req.Addrs {
		if a, err := multiaddr.NewMultiaddr(s); err == nil {
			info.Addrs = append(info.Addrs, a)
		}
	}
	if len(info.Addrs) == 0 {
		return reject("no valid address in the request")
	}

	// A room may only be re-registered by the peer that owns it. Otherwise
	// anyone could take a room over and have the receiver dial them — which
	// the handshake would catch, but only after the real sender had lost
	// the room.
	if existing, taken := r.rooms[req.Nameplate]; taken {
		if existing.info.ID != from {
			return reject(msgInUse)
		}
		// A refresh — typically after a reconnect — brings new addresses
		// but keeps the original clock.
		r.rooms[req.Nameplate] = roomEntry{info: info, createdAt: existing.createdAt}
		return Response{Type: "ok", Nameplate: req.Nameplate}
	}

	if r.roomsOf(from) >= r.maxPerPeer {
		return reject("too many open rooms for one sender")
	}
	if r.maxRooms > 0 && len(r.rooms) >= r.maxRooms && !r.evictAbandoned() {
		return reject("server is full, try again later")
	}

	// A named register for a room the server does not hold is a sender
	// taking its nameplate back after the server forgot it (a restart), so
	// the code its user has already read out keeps working.
	nameplate := req.Nameplate
	if nameplate == "" {
		nameplate = r.freeNameplate()
	}
	r.rooms[nameplate] = roomEntry{info: info, createdAt: time.Now()}
	r.stats.Registered++
	return Response{Type: "ok", Nameplate: nameplate}
}

const nameplateSparsity = 10

// freeNameplate picks an unused nameplate at random from the shortest range
// that is still sparse: at most one in nameplateSparsity of its numbers is
// taken. It starts with two-digit numbers (10-99) and dynamically grows to
// longer ones (100-999, 1000-9999, 10000-99999, etc.) as the server gets
// crowded, keeping codes as short as load permits while preventing mistyped
// numbers from colliding. Called with the lock held.
func (r *Registry) freeNameplate() string {
	lo, hi := 10, 99
	for {
		size := hi - lo + 1
		used := 0
		for np := range r.rooms {
			if n, err := strconv.Atoi(np); err == nil && n >= lo && n <= hi {
				used++
			}
		}
		if (used+1)*nameplateSparsity <= size {
			for {
				np := strconv.Itoa(lo + mrand.IntN(size))
				if _, taken := r.rooms[np]; !taken {
					return np
				}
			}
		}
		nextLo := hi + 1
		nextHi := hi*10 + 9
		if nextHi <= nextLo {
			break
		}
		lo, hi = nextLo, nextHi
	}
	for {
		np := strconv.Itoa(10 + mrand.IntN(1_000_000_000-10))
		if _, taken := r.rooms[np]; !taken {
			return np
		}
	}
}

// evictAbandoned makes space in a full table by dropping the room whose
// owner has been gone longest. Such a room is only waiting in case its
// owner reconnects; a new sender who is here now needs the slot more.
// Without this, connecting, registering and hanging up in a loop would
// fill the table with rooms nobody can reach — each held for its grace
// period — for the price of nothing but connections. Called with the lock
// held; reports whether it found one.
func (r *Registry) evictAbandoned() bool {
	oldest, found := "", false
	var since time.Time
	for np, e := range r.rooms {
		if !e.ownerGone.IsZero() && (!found || e.ownerGone.Before(since)) {
			oldest, since, found = np, e.ownerGone, true
		}
	}
	if found {
		delete(r.rooms, oldest)
		r.stats.Evicted++
	}
	return found
}

// unregister is called with the lock held. Closing a room one does not own
// is reported as success: the caller learns nothing either way, and the
// only honest answer to "make sure this room of mine is gone" when it
// already is, is yes.
func (r *Registry) unregister(from peer.ID, req Request) Response {
	if entry, ok := r.rooms[req.Nameplate]; ok && entry.info.ID == from {
		delete(r.rooms, req.Nameplate)
		r.stats.Unregistered++
	}
	return Response{Type: "ok"}
}

// lookup is called with the lock held. See the comment on the limits above.
func (r *Registry) lookup(from peer.ID, req Request) Response {
	if !ValidNameplate(req.Nameplate) {
		// Cannot be anyone's room, so it is not counted against the peer: a
		// typo in its shape should not use up one of a user's tries.
		r.stats.Rejected++
		return Response{Type: "error", Error: "malformed room number"}
	}

	now := time.Now()
	lc := r.lookups[from].current(now, lookupWindow)
	r.globalMiss = r.globalMiss.current(now, lookupWindow)
	underPressure := r.globalMiss.count >= maxGlobalMisses

	if lc.count >= maxLookups || (underPressure && lc.count > 0) {
		r.stats.LookupsThrottled++
		return Response{Type: "error", Error: "too many lookups, try again in a minute"}
	}
	lc.count++
	r.lookups[from] = lc

	entry, ok := r.rooms[req.Nameplate]
	if !ok {
		r.globalMiss.count++
		r.stats.LookupsNotFound++
		return Response{Type: "not_found"}
	}

	r.stats.LookupsFound++
	if !entry.ownerGone.IsZero() {
		// The room is real but its owner is between connections; its
		// addresses would lead nowhere right now.
		return Response{Type: "error", Error: msgReconnecting}
	}
	addrs := make([]string, len(entry.info.Addrs))
	for i, a := range entry.info.Addrs {
		addrs[i] = a.String()
	}
	return Response{
		Type:       "found",
		PeerID:     entry.info.ID.String(),
		Addrs:      addrs,
		RelayLimit: r.relayLimit,
	}
}

// ownerGone starts the grace period for every room p owns.
func (r *Registry) ownerGone(p peer.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for room, e := range r.rooms {
		if e.info.ID == p && e.ownerGone.IsZero() {
			e.ownerGone = now
			r.rooms[room] = e
		}
	}
}

// ownerBack ends it again.
func (r *Registry) ownerBack(p peer.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for room, e := range r.rooms {
		if e.info.ID == p && !e.ownerGone.IsZero() {
			e.ownerGone = time.Time{}
			r.rooms[room] = e
		}
	}
}

// roomsOf counts a peer's rooms. Called with the lock held.
func (r *Registry) roomsOf(p peer.ID) int {
	n := 0
	for _, e := range r.rooms {
		if e.info.ID == p {
			n++
		}
	}
	return n
}

// ActiveRooms reports how many rooms are currently registered.
func (r *Registry) ActiveRooms() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropExpired()
	return len(r.rooms)
}

// Stats returns a snapshot of the counters.
func (r *Registry) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropExpired()
	s := r.stats
	s.ActiveRooms = len(r.rooms)
	return s
}

// HasPeer reports whether the peer currently owns an active room.
func (r *Registry) HasPeer(p peer.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropExpired()
	return r.roomsOf(p) > 0
}

// AllowReserve and AllowConnect make the Registry usable as the relay
// service's ACL filter (circuitv2 relay.ACLFilter): only peers involved
// in an active room may use the relay, so the server cannot be abused
// as a free traffic bridge by unrelated libp2p nodes.

// AllowReserve permits a relay reservation only for a room owner.
func (r *Registry) AllowReserve(p peer.ID, _ multiaddr.Multiaddr) bool {
	return r.HasPeer(p)
}

// AllowConnect permits relayed connections only towards a room owner
// (the receiver dialing the sender through the relay).
func (r *Registry) AllowConnect(_ peer.ID, _ multiaddr.Multiaddr, dest peer.ID) bool {
	return r.HasPeer(dest)
}

// dropExpired removes rooms past their hour, rooms whose owner has been
// gone longer than the grace period, and stale lookup counters. Called
// with the lock held.
func (r *Registry) dropExpired() {
	now := time.Now()
	for room, e := range r.rooms {
		switch {
		case now.Sub(e.createdAt) > RoomTTL:
			delete(r.rooms, room)
			r.stats.Expired++
		case !e.ownerGone.IsZero() && now.Sub(e.ownerGone) > ownerGrace:
			delete(r.rooms, room)
			r.stats.Abandoned++
		}
	}
	for p, lc := range r.lookups {
		if now.Sub(lc.start) > lookupWindow {
			delete(r.lookups, p)
		}
	}
}
