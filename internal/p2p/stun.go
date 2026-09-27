package p2p

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/p2p/transport/quicreuse"
	"github.com/multiformats/go-multiaddr"
	"github.com/pion/stun/v4"
)

// DefaultSTUNServers lists the public STUN servers used to discover the
// host's external WAN IPv4 address. Cloudflare Anycast provides ultra-low
// latency in Turkey (~3.5 ms via Istanbul POP), while Google STUN serves as a
// reliable global backup.
var DefaultSTUNServers = []string{
	"stun.cloudflare.com:3478",
	"stun.l.google.com:19302",
}

const (
	// stunBudget is how long a round of STUN queries may take in all.
	stunBudget = 3500 * time.Millisecond

	// mappingRefresh is how often the QUIC socket asks STUN where it is
	// while the node runs. A home router forgets an idle UDP mapping after
	// about 30 seconds, and the next packet out may get another port. A
	// query every so often keeps the mapping we advertise alive, and
	// catches it if it moves anyway.
	mappingRefresh = 15 * time.Second
)

// publicAddress is how the internet sees this node, as far as STUN can
// tell. The address factory reads it on every call, so an update applies
// the next time libp2p asks for our addresses — hole punching included.
type publicAddress struct {
	ip atomic.Pointer[net.IP]

	// quic is where the packets of our IPv4 QUIC socket appear to come
	// from, nil if unknown. Its port is the router's, not ours: many
	// routers give an outgoing socket a port of their own choosing.
	quic atomic.Pointer[net.UDPAddr]
}

// IP returns the public IPv4 address, or nil if none was found.
func (p *publicAddress) IP() net.IP {
	if ip := p.ip.Load(); ip != nil {
		return *ip
	}
	return nil
}

func (p *publicAddress) setQUIC(addr *net.UDPAddr) {
	ip := addr.IP
	p.ip.Store(&ip)
	p.quic.Store(addr)
}

// rewrite is the host's address factory. It drops Docker bridge addresses
// and adds the public ones STUN found: the QUIC socket's real mapping, and
// for every other address a guess — the public IP with the local port,
// which is only right on a router that keeps ports.
func (p *publicAddress) rewrite(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
	ip, quic := p.IP(), p.quic.Load()
	var result []multiaddr.Multiaddr
	for _, a := range addrs {
		// Skip Docker container bridge networks (172.16.0.0/12) which
		// bloat the multiaddr list and push useful addrs past MaxAddrs.
		if isDockerAddr(a) {
			continue
		}
		result = append(result, a)
		if ip == nil || (quic != nil && isQUIC4(a)) {
			// No guessing for the QUIC socket when its mapping is known.
			continue
		}
		if pubMA, ok := injectPublicIP(a, ip); ok {
			result = append(result, pubMA)
		}
	}
	if quic != nil {
		if a, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", quic.IP, quic.Port)); err == nil {
			result = append(result, a)
		}
	}
	return multiaddr.Unique(result)
}

// discover fills in p, spending at most ctx on it. Where the QUIC socket
// can be shared, STUN is asked from that socket, which is the one a hole
// punch over QUIC goes out through; the socket is then returned for
// keepMapping to go on using. Otherwise only the public IP is learned, from
// a socket of our own, and nil is returned.
func (p *publicAddress) discover(ctx context.Context, h host.Host, conns *quicreuse.ConnManager, servers []string) net.PacketConn {
	conn, err := quicSocket(h, conns)
	if err == nil {
		addr, err := stunMapping(ctx, conn, servers)
		if err == nil {
			p.setQUIC(addr)
			return conn
		}
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil // the servers did not answer; another socket will not change that
		}
	}
	if ip, err := ResolvePublicIP(ctx, servers); err == nil {
		p.ip.Store(&ip)
	}
	return nil
}

// quicSocket returns a way to send and receive packets that are not QUIC
// on the socket libp2p's QUIC transport listens on for IPv4.
func quicSocket(h host.Host, conns *quicreuse.ConnManager) (net.PacketConn, error) {
	if conns == nil {
		return nil, errors.New("no QUIC transport")
	}
	for _, a := range h.Network().ListenAddresses() {
		if !isQUIC4(a) {
			continue
		}
		ip, err := a.ValueForProtocol(multiaddr.P_IP4)
		if err != nil {
			continue
		}
		port, err := a.ValueForProtocol(multiaddr.P_UDP)
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			continue
		}
		conn, err := conns.SharedNonQUICPacketConn("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: n})
		if err != nil {
			return nil, err
		}
		// quic-go only queues packets that are not QUIC once someone has
		// asked to read one. Ask now, with a deadline already past, so an
		// answer that is quicker than our first read is not dropped.
		_ = conn.SetReadDeadline(time.Now())
		_, _, _ = conn.ReadFrom(make([]byte, 1))
		return conn, nil
	}
	return nil, errors.New("not listening for QUIC over IPv4")
}

// isQUIC4 reports whether a is a plain QUIC address over IPv4, such as
// /ip4/192.168.1.5/udp/4001/quic-v1 — not WebTransport, which listens on a
// socket of its own.
func isQUIC4(a multiaddr.Multiaddr) bool {
	ps := a.Protocols()
	return len(ps) == 3 &&
		ps[0].Code == multiaddr.P_IP4 &&
		ps[1].Code == multiaddr.P_UDP &&
		ps[2].Code == multiaddr.P_QUIC_V1
}

// ResolvePublicIP queries STUN servers in order of priority to determine this
// node's external WAN IPv4 address. It returns the first valid public IP
// discovered, or an error if all servers fail or ctx expires. The queries go
// out from a socket of their own, which is closed afterwards, so the port
// the servers saw means nothing to anyone else.
func ResolvePublicIP(ctx context.Context, servers []string) (net.IP, error) {
	if len(servers) == 0 {
		servers = DefaultSTUNServers
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	addr, err := stunMapping(ctx, conn, servers)
	if err != nil {
		return nil, err
	}
	return addr.IP, nil
}

// stunMapping asks the servers in order where packets from conn appear to
// come from, and returns the first public answer.
func stunMapping(ctx context.Context, conn net.PacketConn, servers []string) (*net.UDPAddr, error) {
	var errs []error
	for i, server := range servers {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		timeout := 1500 * time.Millisecond
		if i > 0 {
			timeout = 2000 * time.Millisecond
		}

		addr, err := stunQuery(ctx, conn, server, timeout)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", server, err))
		case addr.IP.To4() == nil || addr.IP.IsLoopback() || addr.IP.IsPrivate():
			errs = append(errs, fmt.Errorf("%s: returned non-public IP %s", server, addr.IP))
		default:
			return addr, nil
		}
	}

	return nil, fmt.Errorf("could not discover public IP via STUN: %w", errors.Join(errs...))
}

// stunQuery sends an RFC 5389 Binding Request from conn to server and
// returns the address the server saw it come from. conn may carry other
// traffic too; anything that is not the answer is skipped.
func stunQuery(ctx context.Context, conn net.PacketConn, server string, timeout time.Duration) (*net.UDPAddr, error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	raddr, err := resolveUDP4(ctx, server)
	if err != nil {
		return nil, err
	}

	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := conn.WriteTo(req.Raw, raddr); err != nil {
		return nil, err
	}

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return nil, err
		}
		res := new(stun.Message)
		if err := stun.Decode(buf[:n], res); err != nil || res.TransactionID != req.TransactionID {
			continue
		}
		if res.Type != stun.BindingSuccess {
			return nil, fmt.Errorf("STUN server answered %s", res.Type)
		}

		var xorAddr stun.XORMappedAddress
		if err := xorAddr.GetFrom(res); err == nil && xorAddr.IP != nil {
			return &net.UDPAddr{IP: xorAddr.IP, Port: xorAddr.Port}, nil
		}
		var mappedAddr stun.MappedAddress
		if err := mappedAddr.GetFrom(res); err == nil && mappedAddr.IP != nil {
			return &net.UDPAddr{IP: mappedAddr.IP, Port: mappedAddr.Port}, nil
		}
		return nil, errors.New("STUN response contains neither XOR-MAPPED-ADDRESS nor MAPPED-ADDRESS")
	}
}

// resolveUDP4 looks up a host:port as an IPv4 UDP address, within ctx.
func resolveUDP4(ctx context.Context, hostport string) (*net.UDPAddr, error) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	port, err := net.DefaultResolver.LookupPort(ctx, "udp", p)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", h)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s has no IPv4 address", h)
	}
	return &net.UDPAddr{IP: ips[0].AsSlice(), Port: port}, nil
}

// isDockerAddr reports whether a multiaddr belongs to a Docker container
// bridge network (RFC 1918 172.16.0.0/12: 172.16.0.0 to 172.31.255.255).
// Developer machines often have 10+ Docker networks, which otherwise pollute
// the advertised multiaddr list and push useful addresses past MaxAddrs.
func isDockerAddr(a multiaddr.Multiaddr) bool {
	ipStr, err := a.ValueForProtocol(multiaddr.P_IP4)
	if err != nil || ipStr == "" {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31
}

// isLoopbackAddr reports whether a multiaddr points to a loopback interface
// (127.0.0.0/8 or ::1).
func isLoopbackAddr(a multiaddr.Multiaddr) bool {
	if ipStr, err := a.ValueForProtocol(multiaddr.P_IP4); err == nil && ipStr != "" {
		if ip := net.ParseIP(ipStr); ip != nil && ip.IsLoopback() {
			return true
		}
	}
	if ipStr, err := a.ValueForProtocol(multiaddr.P_IP6); err == nil && ipStr != "" {
		if ip := net.ParseIP(ipStr); ip != nil && ip.IsLoopback() {
			return true
		}
	}
	return false
}

// injectPublicIP replaces the IPv4 portion of a local multiaddr with the
// public IP, keeping ports and transport protocols intact.
func injectPublicIP(a multiaddr.Multiaddr, publicIP net.IP) (multiaddr.Multiaddr, bool) {
	if publicIP == nil {
		return nil, false
	}
	ipStr, err := a.ValueForProtocol(multiaddr.P_IP4)
	if err != nil || ipStr == "" {
		return nil, false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.IsLoopback() || ip.Equal(publicIP) {
		return nil, false
	}

	oldPrefix := "/ip4/" + ipStr
	newPrefix := "/ip4/" + publicIP.String()
	newStr := strings.Replace(a.String(), oldPrefix, newPrefix, 1)
	newMA, err := multiaddr.NewMultiaddr(newStr)
	if err != nil {
		return nil, false
	}
	return newMA, true
}
