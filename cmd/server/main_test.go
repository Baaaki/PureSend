package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"puresend/internal/rendezvous"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
)

// TestTunnelClientsAreNotOneClient is the limit that would have bitten on
// the first busy evening. Behind cloudflared every client arrives from the
// tunnel's address — through Docker's bridge, not even loopback — and
// libp2p's defaults allow one address 8 connections. Connections from the
// trusted proxy networks must not share that allowance; everyone else
// still gets libp2p's limits.
func TestTunnelClientsAreNotOneClient(t *testing.T) {
	trusted, err := parsePrefixes(defaultTrustedProxies)
	if err != nil {
		t.Fatal(err)
	}
	rm, err := resourceManager(trusted, rendezvous.MaxRooms)
	if err != nil {
		t.Fatal(err)
	}
	defer rm.Close()

	open := func(ip string) error {
		addr := multiaddr.StringCast(fmt.Sprintf("/ip4/%s/tcp/4001", ip))
		_, err := rm.OpenConnection(network.DirInbound, true, addr)
		return err
	}

	// A container behind Docker's port proxy sees the bridge gateway. 30
	// at once is well past the 8 an address gets by default, and still
	// under the separate cap on connections mid-handshake, which these —
	// never finishing one — would otherwise run into.
	for i := range 30 {
		if err := open("172.18.0.1"); err != nil {
			t.Fatalf("connection %d through the tunnel was refused: %v", i+1, err)
		}
	}

	// A single address on the open internet is still held to the defaults.
	var refused bool
	for range 20 {
		if open("203.0.113.7") != nil {
			refused = true
			break
		}
	}
	if !refused {
		t.Error("an ordinary internet address was let through without a limit")
	}
}

// TestRelayHoldsASlotPerRoom: libp2p allows 8 reservations per address
// and 128 in all. Every room owner needs one, and behind a tunnel they all
// share one address.
func TestRelayHoldsASlotPerRoom(t *testing.T) {
	rc := relayResources(256<<20, 10*time.Minute, 2500)
	for name, got := range map[string]int{
		"MaxReservations":       rc.MaxReservations,
		"MaxReservationsPerIP":  rc.MaxReservationsPerIP,
		"MaxReservationsPerASN": rc.MaxReservationsPerASN,
	} {
		if got != 2500 {
			t.Errorf("%s = %d, want 2500", name, got)
		}
	}
	if rc.Limit == nil || rc.Limit.Data != 256<<20 || rc.Limit.Duration != 10*time.Minute {
		t.Errorf("relay limit = %+v, want the configured one", rc.Limit)
	}
}

func TestListenAddrsIncludesIPv4AndIPv6(t *testing.T) {
	got := listenAddrs(9090, 4001)
	want := []string{
		"/ip4/0.0.0.0/tcp/9090/ws",
		"/ip6/::/tcp/9090/ws",
		"/ip4/0.0.0.0/tcp/4001",
		"/ip6/::/tcp/4001",
		"/ip4/0.0.0.0/udp/4001/quic-v1",
		"/ip6/::/udp/4001/quic-v1",
	}
	if len(got) != len(want) {
		t.Fatalf("listenAddrs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("listenAddrs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParsePrefixes(t *testing.T) {
	got, err := parsePrefixes(" 10.0.0.0/8, ::1/128 ,,")
	if err != nil || len(got) != 2 {
		t.Fatalf("parsePrefixes = %v, %v", got, err)
	}
	if _, err := parsePrefixes("10.0.0.0"); err == nil {
		t.Error("an address without a prefix length was accepted")
	}
}

// TestMetricsRegister guards against a registration panic at startup,
// which is how the duplicate "version" label was found last time.
func TestMetricsRegister(t *testing.T) {
	reg := prometheus.NewRegistry()
	registerMetrics(reg, "12D3KooWtest", rendezvous.NewRegistry())
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range families {
		names = append(names, f.GetName())
	}
	all := strings.Join(names, " ")
	for _, want := range []string{"puresend_active_rooms", "puresend_rooms_abandoned_total", "go_goroutines"} {
		if !strings.Contains(all, want) {
			t.Errorf("metric %s is missing from %s", want, all)
		}
	}
}

// TestUnreadableKeyIsNotReplaced: the key decides the peer ID every
// released client dials. A key file that is there but cannot be read has to
// stop the server, not be swapped for a new key — which is what any read
// error other than "no such file" used to do.
func TestUnreadableKeyIsNotReplaced(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs file permissions that bind the file's owner")
	}
	t.Setenv("FT_IDENTITY_KEY", "")
	path := filepath.Join(t.TempDir(), "server.key")

	priv, _, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Write-only: a read fails where an overwrite would still succeed.
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateKey(path); err == nil {
		t.Error("an unreadable key file was taken as a reason to make a new key")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, original) {
		t.Fatal("the key file was replaced")
	}

	again, _, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if !again.Equals(priv) {
		t.Error("the second start came up with a different identity")
	}
}
