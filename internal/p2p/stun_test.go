package p2p

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/p2p/transport/quicreuse"
	"github.com/multiformats/go-multiaddr"
	"github.com/pion/stun/v4"
	"go.uber.org/fx"
)

// startMockSTUNServer starts a local UDP server that answers RFC 5389
// Binding Requests with an XOR-MAPPED-ADDRESS attribute pointing to targetIP.
func startMockSTUNServer(t *testing.T, targetIP net.IP) (string, func()) {
	t.Helper()
	return startNATSTUNServer(t, targetIP, new(atomic.Int32))
}

// startNATSTUNServer is startMockSTUNServer behind a router that does not
// keep ports: it answers that a query from port p came from port p+shift,
// the way a router that picks its own port for each socket would.
func startNATSTUNServer(t *testing.T, targetIP net.IP, shift *atomic.Int32) (string, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("could not start mock STUN server: %v", err)
	}

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1024)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			msg := stun.New()
			if err := stun.Decode(buf[:n], msg); err != nil {
				continue
			}

			// Respond with a binding success
			res := stun.MustBuild(
				stun.NewTransactionIDSetter(msg.TransactionID),
				stun.BindingSuccess,
				&stun.XORMappedAddress{
					IP:   targetIP,
					Port: shifted(addr.(*net.UDPAddr).Port, int(shift.Load())),
				},
				stun.Fingerprint,
			)
			_, _ = conn.WriteTo(res.Raw, addr)
		}
	}()

	closeFn := func() {
		_ = conn.Close()
		close(done)
	}
	return conn.LocalAddr().String(), closeFn
}

func TestResolvePublicIP_MockServer(t *testing.T) {
	expectedIP := net.ParseIP("203.0.113.50") // RFC 5737 documentation public IP
	addr, cleanup := startMockSTUNServer(t, expectedIP)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ip, err := ResolvePublicIP(ctx, []string{addr})
	if err != nil {
		t.Fatalf("ResolvePublicIP failed: %v", err)
	}
	if !ip.Equal(expectedIP) {
		t.Errorf("got IP %s, want %s", ip, expectedIP)
	}
}

func TestResolvePublicIP_Fallback(t *testing.T) {
	expectedIP := net.ParseIP("198.51.100.25")
	validAddr, cleanup := startMockSTUNServer(t, expectedIP)
	defer cleanup()

	// Dead primary server that drops packets
	deadAddr := "127.0.0.1:1"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ip, err := ResolvePublicIP(ctx, []string{deadAddr, validAddr})
	if err != nil {
		t.Fatalf("fallback ResolvePublicIP failed: %v", err)
	}
	if !ip.Equal(expectedIP) {
		t.Errorf("got IP %s, want %s", ip, expectedIP)
	}
}

func TestIsDockerAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"/ip4/172.17.0.1/tcp/4001", true},
		{"/ip4/172.28.0.1/udp/5000/quic-v1", true},
		{"/ip4/172.16.0.1/tcp/80", true},
		{"/ip4/172.31.255.254/tcp/80", true},
		{"/ip4/172.15.0.1/tcp/80", false}, // outside 172.16-31
		{"/ip4/172.32.0.1/tcp/80", false}, // outside 172.16-31
		{"/ip4/192.168.1.104/tcp/4001", false},
		{"/ip4/10.0.0.5/tcp/4001", false},
		{"/ip4/127.0.0.1/tcp/4001", false},
		{"/ip4/188.119.40.165/tcp/4001", false},
		{"/ip6/::1/tcp/4001", false},
	}

	for _, tc := range cases {
		ma, err := multiaddr.NewMultiaddr(tc.addr)
		if err != nil {
			t.Fatalf("invalid multiaddr %s: %v", tc.addr, err)
		}
		got := isDockerAddr(ma)
		if got != tc.want {
			t.Errorf("isDockerAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"/ip4/127.0.0.1/tcp/4001", true},
		{"/ip4/127.0.0.2/udp/5000/quic-v1", true},
		{"/ip6/::1/tcp/4001", true},
		{"/ip4/192.168.1.104/tcp/4001", false},
		{"/ip4/172.17.0.1/tcp/4001", false},
		{"/ip4/188.119.40.165/tcp/4001", false},
		{"/ip6/2a02:ff0::1/tcp/4001", false},
	}

	for _, tc := range cases {
		ma, err := multiaddr.NewMultiaddr(tc.addr)
		if err != nil {
			t.Fatalf("invalid multiaddr %s: %v", tc.addr, err)
		}
		got := isLoopbackAddr(ma)
		if got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestInjectPublicIP(t *testing.T) {
	pubIP := net.ParseIP("188.119.40.165")

	// LAN addr should be converted to public IP
	lanMA := multiaddr.StringCast("/ip4/192.168.1.104/udp/45000/quic-v1")
	injected, ok := injectPublicIP(lanMA, pubIP)
	if !ok {
		t.Fatal("injectPublicIP should succeed for LAN addr")
	}
	want := "/ip4/188.119.40.165/udp/45000/quic-v1"
	if injected.String() != want {
		t.Errorf("got %s, want %s", injected.String(), want)
	}

	// Loopback should NOT be converted
	loopMA := multiaddr.StringCast("/ip4/127.0.0.1/tcp/4001")
	if _, ok := injectPublicIP(loopMA, pubIP); ok {
		t.Error("injectPublicIP should not convert loopback")
	}

	// Already public IP should not be duplicated
	alreadyPub := multiaddr.StringCast("/ip4/188.119.40.165/tcp/4001")
	if _, ok := injectPublicIP(alreadyPub, pubIP); ok {
		t.Error("injectPublicIP should not convert already identical public IP")
	}

	// IPv6 should be ignored by injectPublicIP
	ipv6MA := multiaddr.StringCast("/ip6/2a02::1/tcp/4001")
	if _, ok := injectPublicIP(ipv6MA, pubIP); ok {
		t.Error("injectPublicIP should not convert IPv6 addr")
	}
}

func TestResolvePublicIP_LiveServer(t *testing.T) {
	// Skip if running in short/sandbox mode without external network
	if testing.Short() {
		t.Skip("skipping live STUN test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ip, err := ResolvePublicIP(ctx, DefaultSTUNServers)
	if err != nil {
		t.Skipf("skipping live test due to network or firewall: %v", err)
		return
	}
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() {
		t.Skipf("skipping live test: unexpected public IP: %v", ip)
		return
	}
}

func TestLiveNodeDiscoversPublicIPAndSynthesizesAddrs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}

	serverAddr := "/dns4/rendezvous.madebybaki.com/tcp/443/tls/ws/p2p/12D3KooWJdXaT1FN4UGLCrrTpdqvpo7cqrJZK6tHvbUPQbJ6APtK"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	node, err := New(ctx, []string{serverAddr})
	if err != nil {
		t.Skipf("skipping live test due to network or firewall: %v", err)
		return
	}
	defer node.Close()

	pubIP := node.PublicIP()
	if pubIP == nil {
		t.Skip("skipping live test: STUN did not discover public IP (UDP STUN likely blocked by network)")
		return
	}
	t.Logf("Discovered public IP via STUN: %s", pubIP)

	hasPublicAddr := false
	for _, a := range node.Addrs() {
		if strings.Contains(a.String(), pubIP.String()) {
			hasPublicAddr = true
			t.Logf("Found synthesized public addr: %s", a)
		}
	}
	if !hasPublicAddr {
		t.Errorf("did not find any advertised multiaddr with discovered public IP %s", pubIP)
	}
}

// shifted is the port a router that adds shift to every port gives p.
func shifted(p, shift int) int {
	return (p+shift-1)%65535 + 1
}

// quicListenPort is the port a host listens on for QUIC over IPv4.
func quicListenPort(t *testing.T, addrs []multiaddr.Multiaddr) int {
	t.Helper()
	for _, a := range addrs {
		if !isQUIC4(a) {
			continue
		}
		p, err := a.ValueForProtocol(multiaddr.P_UDP)
		if err != nil {
			t.Fatal(err)
		}
		var port int
		if _, err := fmt.Sscan(p, &port); err != nil {
			t.Fatal(err)
		}
		return port
	}
	t.Fatalf("no QUIC listener over IPv4 in %v", addrs)
	return 0
}

func hasAddr(addrs []multiaddr.Multiaddr, want string) bool {
	for _, a := range addrs {
		if a.String() == want {
			return true
		}
	}
	return false
}

func TestRewrite(t *testing.T) {
	in := []multiaddr.Multiaddr{
		multiaddr.StringCast("/ip4/192.168.1.104/udp/45000/quic-v1"),
		multiaddr.StringCast("/ip4/192.168.1.104/tcp/41000"),
		multiaddr.StringCast("/ip4/192.168.1.104/udp/46000/quic-v1/webtransport"),
		multiaddr.StringCast("/ip4/172.17.0.1/udp/45000/quic-v1"),
		multiaddr.StringCast("/ip4/127.0.0.1/udp/45000/quic-v1"),
	}
	strs := func(addrs []multiaddr.Multiaddr) []string {
		var out []string
		for _, a := range addrs {
			out = append(out, a.String())
		}
		return out
	}

	var p publicAddress
	if got := strs(p.rewrite(in)); len(got) != 4 {
		t.Errorf("with nothing from STUN: got %v, want the four non-Docker addresses as they are", got)
	}

	// The public IP alone: every address gets a public twin on the same port.
	ip := net.ParseIP("188.119.40.165")
	p.ip.Store(&ip)
	out := p.rewrite(in)
	for _, want := range []string{
		"/ip4/188.119.40.165/udp/45000/quic-v1",
		"/ip4/188.119.40.165/tcp/41000",
		"/ip4/188.119.40.165/udp/46000/quic-v1/webtransport",
	} {
		if !hasAddr(out, want) {
			t.Errorf("with the IP alone: %s missing from %v", want, strs(out))
		}
	}

	// The QUIC socket's mapping: QUIC gets the router's port instead of
	// ours; the others keep their guess.
	p.setQUIC(&net.UDPAddr{IP: ip, Port: 2332})
	out = p.rewrite(in)
	if !hasAddr(out, "/ip4/188.119.40.165/udp/2332/quic-v1") {
		t.Errorf("mapped QUIC address missing from %v", strs(out))
	}
	if hasAddr(out, "/ip4/188.119.40.165/udp/45000/quic-v1") {
		t.Errorf("QUIC still advertised on the local port: %v", strs(out))
	}
	if !hasAddr(out, "/ip4/192.168.1.104/udp/45000/quic-v1") {
		t.Errorf("the LAN address must stay for peers on the same network: %v", strs(out))
	}
	if !hasAddr(out, "/ip4/188.119.40.165/tcp/41000") {
		t.Errorf("TCP guess missing from %v", strs(out))
	}
	for _, a := range out {
		if isDockerAddr(a) {
			t.Errorf("Docker address advertised: %s", a)
		}
	}
}

// TestNewAdvertisesTheRoutersPort is the case that sent transfers through
// the relay: a router that gives the QUIC socket a port of its own. The
// node must ask STUN from that socket and advertise the router's port — the
// one a hole punch can reach — not its own.
func TestNewAdvertisesTheRoutersPort(t *testing.T) {
	publicIP := net.ParseIP("203.0.113.7")
	shift := new(atomic.Int32)
	shift.Store(1000)
	stunAddr, stopSTUN := startNATSTUNServer(t, publicIP, shift)
	defer stopSTUN()

	server, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addr := fmt.Sprintf("%s/p2p/%s", server.Addrs()[0], server.ID())
	node, err := New(ctx, []string{addr}, WithSTUNServers([]string{stunAddr}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer node.Close()

	if !node.PublicIP().Equal(publicIP) {
		t.Errorf("PublicIP = %v, want %v", node.PublicIP(), publicIP)
	}
	port := quicListenPort(t, node.host.Network().ListenAddresses())
	want := fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", publicIP, shifted(port, 1000))
	if !hasAddr(node.Addrs(), want) {
		t.Errorf("%s missing from %v", want, node.Addrs())
	}
	wrong := fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", publicIP, port)
	if hasAddr(node.Addrs(), wrong) {
		t.Errorf("still advertising the local port: %s", wrong)
	}
}

// TestKeepMappingFollowsTheRouter covers a router that forgets an idle
// mapping and gives the socket another port: the next refresh must put the
// new port in the address list.
func TestKeepMappingFollowsTheRouter(t *testing.T) {
	publicIP := net.ParseIP("203.0.113.8")
	shift := new(atomic.Int32)
	shift.Store(7)
	stunAddr, stopSTUN := startNATSTUNServer(t, publicIP, shift)
	defer stopSTUN()

	public := new(publicAddress)
	var conns *quicreuse.ConnManager
	h, err := libp2p.New(
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/udp/0/quic-v1"),
		libp2p.AddrsFactory(public.rewrite),
		libp2p.WithFxOption(fx.Populate(&conns)),
	)
	if err != nil {
		t.Fatal(err)
	}
	n := newTestNode(t)
	n.host.Close()
	n.host, n.public = h, public
	defer n.Close()

	ctx, cancel := context.WithTimeout(context.Background(), stunBudget)
	conn := public.discover(ctx, h, conns, []string{stunAddr})
	cancel()
	if conn == nil {
		t.Fatal("no mapping found from the QUIC socket")
	}
	port := quicListenPort(t, h.Network().ListenAddresses())
	if got := public.quic.Load(); got == nil || got.Port != shifted(port, 7) {
		t.Fatalf("mapping = %v, want port %d", got, shifted(port, 7))
	}

	shift.Store(9)
	go n.keepMapping(conn, []string{stunAddr}, 20*time.Millisecond)
	want := fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", publicIP, shifted(port, 9))
	deadline := time.Now().Add(5 * time.Second)
	for !hasAddr(h.Addrs(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never showed up in %v", want, h.Addrs())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
