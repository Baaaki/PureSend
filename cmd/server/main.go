// Server: the rendezvous point where peers find each other.
//
// It has two jobs:
//  1. Rendezvous: the sender registers a room code, the receiver looks it up.
//  2. Circuit Relay v2: the receiver's *first* connection to the sender is
//     established through this server; libp2p's hole punching mechanism
//     (DCUtR) then uses that bridge to negotiate a direct connection.
//     Once the direct connection is up the files bypass this server
//     entirely.
//
// It is deliberately not a trusted party. It never sees file contents, and
// it is only ever told the nameplate of a room code — the number — never
// its secret words. The two clients prove to each other that they hold the
// whole code before anything moves, so the server cannot pass off a peer
// of its own as either of them without guessing the words. What it does
// see is metadata: which peers meet, and when.
//
// # Running behind Cloudflare Tunnel
//
// cloudflared only proxies HTTP/WebSocket to the public internet — a raw
// TCP or QUIC port cannot be exposed that way. So the primary listener is
// libp2p's WebSocket transport, which Cloudflare proxies natively:
//
//	cloudflared ingress:  rendezvous.example.com -> http://localhost:8080
//	server listens on:    /ip4/0.0.0.0/tcp/8080/ws
//	clients dial:         /dns4/rendezvous.example.com/tcp/443/tls/ws/p2p/<PeerID>
//
// TLS is terminated by Cloudflare, which is why the listener itself is
// plain /ws while clients dial /tls/ws.
//
//	go run ./cmd/server -ws-port 8080 \
//	  -announce /dns4/rendezvous.example.com/tcp/443/tls/ws
//
// Behind a tunnel every client arrives from the tunnel's own address, so
// the per-address limits libp2p applies by default would treat all of them
// as one very busy client. -trusted-proxies names the networks such
// connections come from; see resourceManager.
//
// # Running with a plain forwarded port
//
// If you can forward a port on your router instead, -port enables the raw
// TCP+QUIC listeners and no tunnel is needed:
//
//	go run ./cmd/server -port 4001
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"puresend/internal/rendezvous"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/x/rate"
	"github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Build information, filled in at build time with -ldflags -X.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// defaultTrustedProxies are the networks a tunnel or reverse proxy on the
// same machine connects from: loopback when cloudflared runs on the host,
// a private bridge address when the server runs in a container.
const defaultTrustedProxies = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"

func main() {
	wsPort := flag.Int("ws-port", 8080, "WebSocket port (the Cloudflare Tunnel target); 0 disables it")
	port := flag.Int("port", 0, "raw TCP+QUIC port for setups with a forwarded port; 0 disables it")
	healthAddr := flag.String("health-addr", envOr("FT_HEALTH_ADDR", "127.0.0.1:8081"),
		"address for the /health and /metrics endpoints; empty disables them. Keep it off the internet")
	keyPath := flag.String("key", envOr("FT_KEY_PATH", "server.key"), "path of the identity key file")
	announce := flag.String("announce", os.Getenv("FT_ANNOUNCE"), "comma-separated public multiaddrs to advertise, e.g. /dns4/host/tcp/443/tls/ws")
	proxies := flag.String("trusted-proxies", envOr("FT_TRUSTED_PROXIES", defaultTrustedProxies),
		"comma-separated networks clients arrive through (the tunnel or proxy); not limited per address")
	relayData := flag.Int64("relay-data", 256<<20, "max bytes relayed per connection when hole punching fails")
	relayDuration := flag.Duration("relay-duration", 10*time.Minute, "max lifetime of a relayed connection")
	roomsPerPeer := flag.Int("rooms-per-peer", rendezvous.DefaultMaxRoomsPerPeer, "how many rooms one sender may hold at once")
	maxRooms := flag.Int("max-rooms", rendezvous.MaxRooms, "maximum concurrent rooms the server will hold (0 for unlimited)")
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("puresend-server %s\n", version)
		fmt.Printf("  commit:  %s\n", commit)
		fmt.Printf("  built:   %s\n", date)
		fmt.Printf("  go:      %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}

	if *wsPort == 0 && *port == 0 {
		log.Fatal("nothing to listen on: set -ws-port or -port")
	}

	// The server's identity (peer ID) must stay the same across restarts
	// so the address baked into released clients remains valid. We save
	// the private key to disk and reload it on the next start.
	priv, keySource, err := loadOrCreateKey(*keyPath)
	if err != nil {
		log.Fatalf("could not prepare identity key: %v", err)
	}

	announced, err := parseAnnounce(*announce)
	if err != nil {
		log.Fatalf("could not parse -announce (%v). Expected comma-separated multiaddrs such as /dns4/example.com/tcp/443/tls/ws", err)
	}
	trusted, err := parsePrefixes(*proxies)
	if err != nil {
		log.Fatalf("could not parse -trusted-proxies: %v", err)
	}
	rm, err := resourceManager(trusted)
	if err != nil {
		log.Fatalf("could not set up connection limits: %v", err)
	}

	registry := rendezvous.NewRegistry(
		rendezvous.WithRelayLimit(*relayData),
		rendezvous.WithMaxRoomsPerPeer(*roomsPerPeer),
		rendezvous.WithMaxRooms(*maxRooms),
	)

	// One registry for everything /metrics serves: ours, and libp2p's own —
	// the relay service, the connection limits, the transports. libp2p
	// records all of that anyway; handed a registry, it records it where
	// someone can read it.
	metrics := prometheus.NewRegistry()

	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(listenAddrs(*wsPort, *port)...),
		libp2p.ResourceManager(rm),
		libp2p.PrometheusRegisterer(metrics),
		// The relay service normally waits until the node has verified it
		// is publicly reachable. Behind a tunnel that probe cannot
		// succeed (the tunnel is outbound-only), so declare it public.
		libp2p.ForceReachabilityPublic(),
		// Serve AutoNAT v2 so clients can discover whether their own
		// addresses are reachable, which is what makes them advertise a
		// sensible address set for hole punching.
		libp2p.EnableAutoNATv2(),
		// Relay v2. The registry acts as the ACL: only peers with an
		// active room may use the relay, so strangers cannot burn our
		// bandwidth. Limits are bounded rather than infinite — the relay
		// exists to bootstrap hole punching (a few KB of DCUtR
		// coordination), and a bounded fallback keeps a failed hole punch
		// from streaming gigabytes through the tunnel.
		libp2p.EnableRelayService(
			relay.WithResources(relayResources(*relayData, *relayDuration)),
			relay.WithACL(registry),
		),
	}
	if len(announced) > 0 {
		// Behind a tunnel the host's own socket addresses (0.0.0.0, the
		// container's private IP) are useless to the outside world.
		// Advertise only what clients can actually reach.
		opts = append(opts, libp2p.AddrsFactory(func([]multiaddr.Multiaddr) []multiaddr.Multiaddr {
			return announced
		}))
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "bind:") {
			log.Fatalf("could not start libp2p host: address already in use (check if ports -ws-port %d or -port %d are already in use by another process): %v", *wsPort, *port, err)
		}
		log.Fatalf("could not start libp2p host: %v", err)
	}
	defer h.Close() //nolint:errcheck // shutting down; nothing to recover

	registry.Serve(h)
	registerMetrics(metrics, h.ID().String(), registry)

	stopHealth, err := startHealthServer(*healthAddr, h.ID().String(), registry, metrics)
	if err != nil {
		// Fatal on purpose. A server whose health endpoint never came up
		// looks alive to its supervisor's restart policy and dead to its
		// health check, forever, and the only clue is one line in a log
		// nobody is reading.
		log.Fatalf("could not start the health server: %v", err)
	}
	defer stopHealth()

	fmt.Printf("Rendezvous + relay server %s is running.\n", version)
	fmt.Println()
	fmt.Println("  Peer ID:", h.ID())
	fmt.Println("  Identity from:", keySource)
	fmt.Println()

	if len(announced) > 0 {
		fmt.Println("Client address (bake this into the client build):")
		for _, addr := range announced {
			fmt.Printf("  %s/p2p/%s\n", addr, h.ID())
		}
	} else {
		// Without -announce (or FT_ANNOUNCE) the only addresses we know
		// are the ones this process is bound to — behind a tunnel or in a
		// container those are private and useless to a client. Say so,
		// rather than printing them under a heading that invites someone
		// to ship them.
		fmt.Println("No public address configured (-announce / FT_ANNOUNCE).")
		fmt.Println("Clients must dial the hostname your tunnel serves, e.g.")
		fmt.Printf("  /dns4/<your-host>/tcp/443/tls/ws/p2p/%s\n", h.ID())
		fmt.Println()
		fmt.Println("Bound to (private, not for clients):")
		for _, addr := range h.Addrs() {
			fmt.Printf("  %s\n", addr)
		}
	}
	fmt.Println()
	fmt.Printf("Relay fallback limit: %s per connection, %s max lifetime\n",
		formatBytes(*relayData), *relayDuration)
	if *healthAddr != "" {
		fmt.Printf("Health: http://%s/health   Metrics: http://%s/metrics\n", *healthAddr, *healthAddr)
	}
	fmt.Println("Press Ctrl+C to stop.")

	// Keep running until Ctrl+C or SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down.")
}

// listenAddrs builds the listen multiaddrs for the enabled ports.
func listenAddrs(wsPort, rawPort int) []string {
	var addrs []string
	if wsPort > 0 {
		// Plain /ws, not /wss: TLS is terminated by Cloudflare (or any
		// other reverse proxy) in front of us.
		addrs = append(addrs, fmt.Sprintf("/ip4/0.0.0.0/tcp/%d/ws", wsPort))
	}
	if rawPort > 0 {
		addrs = append(addrs,
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", rawPort),
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", rawPort),
		)
	}
	return addrs
}

// parseAnnounce parses the comma-separated -announce value.
func parseAnnounce(s string) ([]multiaddr.Multiaddr, error) {
	var out []multiaddr.Multiaddr
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		a, err := multiaddr.NewMultiaddr(part)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", part, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// parsePrefixes parses a comma-separated list of networks.
func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// relayResources bounds what a relayed connection may consume, and how
// many peers may hold a slot.
//
// libp2p's defaults allow 8 reservations per IP address and 128 in all.
// Behind a tunnel every client shares one address, so the ninth sender
// waiting for a friend used to be refused a slot — "the meeting point
// refused to hold a slot for us" — on the first busy evening. Every room
// owner holds exactly one reservation, so the room table already bounds
// them; the relay's ACL admits only room owners in the first place.
func relayResources(data int64, duration time.Duration) relay.Resources {
	rc := relay.DefaultResources()
	rc.Limit = &relay.RelayLimit{Duration: duration, Data: data}
	rc.MaxReservations = rendezvous.MaxRooms
	rc.MaxReservationsPerIP = rendezvous.MaxRooms
	rc.MaxReservationsPerASN = rendezvous.MaxRooms
	return rc
}

// resourceManager is libp2p's connection and memory limiter, set up for a
// server that sits behind a tunnel.
//
// Out of the box libp2p allows each IP address 8 connections at once and
// new ones at a trickle (a burst of 16, then one every five seconds), and
// exempts only loopback. That is sound for a node on the open internet
// and ruinous behind a proxy: cloudflared on the host reaches a container
// through Docker's bridge, not loopback, so every client in the world
// would share those 8 connections. Connections from the trusted proxy
// networks are therefore not limited per address — the tunnel is where
// per-client limits belong (see the README) — while everyone else keeps
// libp2p's defaults.
//
// The server-wide ceiling is raised too: every waiting sender holds one
// connection for up to an hour, and the default of a hundred or so would
// run out long before the room table does.
func resourceManager(trusted []netip.Prefix) (network.ResourceManager, error) {
	limits := rcmgr.DefaultLimits
	libp2p.SetDefaultServiceLimits(&limits)
	limits.SystemBaseLimit.ConnsInbound = max(limits.SystemBaseLimit.ConnsInbound, 2*rendezvous.MaxRooms)
	limits.SystemBaseLimit.Conns = max(limits.SystemBaseLimit.Conns, 4*rendezvous.MaxRooms)

	v4 := append([]rcmgr.NetworkPrefixLimit(nil), rcmgr.DefaultNetworkPrefixLimitV4...)
	v6 := append([]rcmgr.NetworkPrefixLimit(nil), rcmgr.DefaultNetworkPrefixLimitV6...)
	unlimited := []rate.PrefixLimit{
		{Prefix: netip.MustParsePrefix("127.0.0.0/8")},
		{Prefix: netip.MustParsePrefix("::1/128")},
	}
	for _, p := range trusted {
		limit := rcmgr.NetworkPrefixLimit{Network: p, ConnCount: math.MaxInt}
		if p.Addr().Is4() {
			v4 = append(v4, limit)
		} else {
			v6 = append(v6, limit)
		}
		unlimited = append(unlimited, rate.PrefixLimit{Prefix: p}) // zero rate: no limit
	}

	return rcmgr.NewResourceManager(
		rcmgr.NewFixedLimiter(limits.AutoScale()),
		rcmgr.WithNetworkPrefixLimit(v4, v6),
		rcmgr.WithConnRateLimiters(&rate.Limiter{
			NetworkPrefixLimits: unlimited,
			// libp2p's own defaults, for everyone not behind the proxy.
			SubnetRateLimiter: rate.SubnetLimiter{
				IPv4SubnetLimits: []rate.SubnetLimit{
					{PrefixLength: 32, Limit: rate.Limit{RPS: 0.2, Burst: 16}},
				},
				IPv6SubnetLimits: []rate.SubnetLimit{
					{PrefixLength: 56, Limit: rate.Limit{RPS: 0.2, Burst: 16}},
					{PrefixLength: 48, Limit: rate.Limit{RPS: 0.5, Burst: 80}},
				},
				GracePeriod: time.Minute,
			},
		}),
	)
}

// startHealthServer exposes /health for a supervisor to probe and
// /metrics for Prometheus. It binds the port before returning, so a port
// that is already taken is reported to the caller rather than discovered
// later in a log line.
//
// It binds to loopback unless told otherwise. /metrics says how busy the
// server is and which build it runs; nobody outside needs to know either,
// and a binary started by hand on a machine with a LAN should not publish
// them to it. The container image asks for all interfaces explicitly,
// because inside a container that only reaches the container's own
// network, and the compose file decides what the host exposes.
func startHealthServer(addr, peerID string, registry *rendezvous.Registry, metrics *prometheus.Registry) (func(), error) {
	if addr == "" {
		return func() {}, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"status\":\"ok\",\"version\":%q,\"peer_id\":%q,\"active_rooms\":%d}\n",
			version, peerID, registry.ActiveRooms())
	})
	mux.Handle("/metrics", promhttp.HandlerFor(metrics, promhttp.HandlerOpts{}))

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("health server stopped: %v", err)
		}
	}()

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}

// registerMetrics adds the registry's counters, and the standard Go
// runtime and process collectors, to the metrics libp2p already reports.
//
// The question operators ask most — how many users fail to open a direct
// route and fall back to the relay — is answered by libp2p's relay
// metrics, not these: libp2p_relaysvc_data_transferred_bytes_total grows
// by a few kilobytes for a hole punch the relay merely introduced, and by
// megabytes for a transfer that had to ride through it. That rate is what
// decides whether -relay-data is set sensibly. libp2p_rcmgr_blocked_resources
// says when the connection limits bite.
func registerMetrics(r *prometheus.Registry, peerID string, reg *rendezvous.Registry) {
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// Only our own metrics carry these labels. The Go collector already
	// publishes a "version" label of its own on go_info, and wrapping it
	// with a second one is a registration panic at startup.
	wrapped := prometheus.WrapRegistererWith(
		prometheus.Labels{"peer_id": peerID, "build": version}, r)

	// Counters for things that only ever go up, gauges for things that
	// move both ways — the distinction is what lets rate() mean anything.
	counter := func(name, help string, read func(rendezvous.Stats) uint64) {
		wrapped.MustRegister(prometheus.NewCounterFunc(
			prometheus.CounterOpts{Namespace: "puresend", Name: name, Help: help},
			func() float64 { return float64(read(reg.Stats())) },
		))
	}

	wrapped.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace: "puresend",
			Name:      "active_rooms",
			Help:      "Room codes currently registered.",
		},
		func() float64 { return float64(reg.Stats().ActiveRooms) },
	))

	counter("rooms_opened_total", "Rooms registered since start.",
		func(s rendezvous.Stats) uint64 { return s.Registered })
	counter("rooms_closed_total", "Rooms retired by their owner after a completed transfer.",
		func(s rendezvous.Stats) uint64 { return s.Unregistered })
	counter("rooms_expired_total", "Rooms dropped at the end of their hour.",
		func(s rendezvous.Stats) uint64 { return s.Expired })
	counter("rooms_abandoned_total", "Rooms dropped because their owner disconnected and did not come back.",
		func(s rendezvous.Stats) uint64 { return s.Abandoned })
	counter("rooms_evicted_total", "Rooms whose owner had disconnected, dropped early to make space in a full table.",
		func(s rendezvous.Stats) uint64 { return s.Evicted })
	counter("lookups_found_total", "Lookups that matched a live room.",
		func(s rendezvous.Stats) uint64 { return s.LookupsFound })
	counter("lookups_not_found_total", "Lookups for a room number nobody holds: a wrong or expired code.",
		func(s rendezvous.Stats) uint64 { return s.LookupsNotFound })
	counter("lookups_throttled_total", "Lookups refused by the rate limiter.",
		func(s rendezvous.Stats) uint64 { return s.LookupsThrottled })
	counter("requests_rejected_total", "Requests refused for any other reason.",
		func(s rendezvous.Stats) uint64 { return s.Rejected })
}

// envOr returns the environment variable's value, or fallback when it is
// unset or empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadOrCreateKey resolves the server's identity, which decides its peer
// ID and therefore whether the clients already released can still reach
// it. Three sources, in order:
//
//  1. FT_IDENTITY_KEY — the base64 of a marshalled private key. Survives
//     anything that happens to the container's disk, which is what makes
//     it the right choice on a platform that mounts an anonymous volume
//     (or none) and hands you a fresh one on every redeploy.
//  2. the key file, when it exists.
//  3. a freshly generated key, written to the file.
//
// Print an existing key in the form (1) wants with:
//
//	base64 -w0 /data/server.key
func loadOrCreateKey(path string) (crypto.PrivKey, string, error) {
	if encoded := os.Getenv("FT_IDENTITY_KEY"); encoded != "" {
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, "", fmt.Errorf("FT_IDENTITY_KEY is not valid base64: %w", err)
		}
		priv, err := crypto.UnmarshalPrivateKey(data)
		if err != nil {
			return nil, "", fmt.Errorf("FT_IDENTITY_KEY is not a valid key: %w", err)
		}
		return priv, "FT_IDENTITY_KEY", nil
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if fi, serr := os.Stat(path); serr == nil && runtime.GOOS != "windows" {
			if fi.Mode().Perm()&0o077 != 0 {
				log.Printf("WARNING: identity key %s has open permissions (%04o); consider 'chmod 0600 %s'", path, fi.Mode().Perm(), path)
			}
		}
		priv, err := crypto.UnmarshalPrivateKey(data)
		if err != nil {
			return nil, "", fmt.Errorf("%s is not a valid key: %w", path, err)
		}
		return priv, path, nil
	case !errors.Is(err, fs.ErrNotExist):
		// The file is there and could not be read — permissions, a disk
		// error. A new key would change the peer ID and strand every client
		// already released, so this is a reason to stop, not to start over.
		return nil, "", fmt.Errorf("could not read %s: %w", path, err)
	}

	priv, _, err := crypto.GenerateEd25519Key(nil) // nil -> uses crypto/rand
	if err != nil {
		return nil, "", err
	}
	data, err = crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, "", err
	}
	// O_EXCL: a key that appeared since the read above is never replaced.
	// 0600: only the owner may read the key.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, "", err
	}
	if err := f.Close(); err != nil {
		return nil, "", err
	}
	return priv, path + " (newly generated)", nil
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
