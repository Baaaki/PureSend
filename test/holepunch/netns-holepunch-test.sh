#!/usr/bin/env bash
#
# Hole punching between two peers behind home routers that do not keep
# ports: a socket going out from port 40000 shows up on the internet as,
# say, 41024 — the same 41024 whoever it talks to, but not 40000. Many home
# routers work this way, and on them a peer that advertises its public IP
# with its *local* port offers the other side an address that leads
# nowhere: every hole punch fails and the transfer falls back to the relay.
#
# The meeting point sits behind a proxy, as it does behind Cloudflare
# Tunnel in production, so it cannot tell a peer what its address looks
# like from outside. STUN is all a peer has to go on.
#
# It needs no root: unprivileged user + network namespaces do the whole
# thing. Run it with `make test-holepunch`, which builds the binaries first
# and then runs, in effect:
#
#     FT_BIN_DIR=bin unshare -Urnm --map-root-user test/holepunch/netns-holepunch-test.sh
#
# FT_PORT_XOR (default 1024) is what the routers XOR a port with to move
# it; 0 makes them keep ports, which hole punching has always managed. FT_EXPECT
# (default "direct") is how the peers should end up connected: "relay"
# turns the script around, to show that a client without the fix fails.
set -euo pipefail

# Numbers below are parsed from Prometheus output ("1.6e+07"); under a
# locale that writes decimals with a comma, awk and printf misread them.
export LC_ALL=C

SIZE_MB="${FT_TEST_SIZE_MB:-4}"
MOVE="${FT_PORT_XOR:-1024}"
EXPECT="${FT_EXPECT:-direct}"
WORK="$(mktemp -d)"
PIDS=()
cleanup() {
  for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$WORK"
}
trap cleanup EXIT

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

if [ "$(id -u)" != "0" ]; then
  echo "this script expects to run inside 'unshare -Urnm --map-root-user'" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# The topology
#
#   nsa 192.168.1.2 ── rtra ── 5.0.0.2 ─┐                   ┌─ 5.0.0.3 ── rtrb ── nsb 192.168.2.2
#      sender          NAT              ├─ br0 · 5.0.0.1 ───┤             NAT         receiver
#                                       │  "the internet":  │
#                                       │  STUN, proxy,     │
#                                       │  server + relay   │
#
# Each router masquerades its LAN, lets in only replies to what went out,
# and moves every UDP and TCP port on the way out — XORs it with MOVE — and
# back on the way in. The addresses are public ones on purpose: libp2p only offers
# public addresses for hole punching.
# ---------------------------------------------------------------------------
log "building the topology (routers XOR ports with $MOVE)"

# `ip netns` keeps its state in /run/netns, which we cannot write to as a
# mapped root. The mount namespace from -m lets us put a tmpfs there
# without touching the real /run.
mount --make-rprivate / 2>/dev/null || true
mount -t tmpfs tmpfs /run
mkdir -p /run/netns

ip link add br0 type bridge
ip addr add 5.0.0.1/24 dev br0
ip link set br0 up
ip link set lo up

for side in "a:2:1" "b:3:2"; do
  IFS=: read -r name wan lan <<<"$side"
  rtr="rtr$name"
  peer="ns$name"
  ip netns add "$rtr"
  ip netns add "$peer"
  ip netns exec "$rtr" ip link set lo up
  ip netns exec "$peer" ip link set lo up

  ip link add "wan-$name" type veth peer name "up-$name"
  ip link set "wan-$name" master br0
  ip link set "wan-$name" up
  ip link set "up-$name" netns "$rtr"
  ip netns exec "$rtr" ip addr add "5.0.0.$wan/24" dev "up-$name"
  ip netns exec "$rtr" ip link set "up-$name" up

  ip link add "lan-$name" type veth peer name "eth-$name"
  ip link set "lan-$name" netns "$rtr"
  ip link set "eth-$name" netns "$peer"
  ip netns exec "$rtr" ip addr add "192.168.$lan.1/24" dev "lan-$name"
  ip netns exec "$rtr" ip link set "lan-$name" up
  ip netns exec "$peer" ip addr add "192.168.$lan.2/24" dev "eth-$name"
  ip netns exec "$peer" ip link set "eth-$name" up
  ip netns exec "$peer" ip route add default via "192.168.$lan.1"

  ip netns exec "$rtr" sysctl -qw net.ipv4.ip_forward=1
  ip netns exec "$rtr" iptables -t nat -A POSTROUTING -o "up-$name" -j MASQUERADE
  ip netns exec "$rtr" iptables -A FORWARD -i "up-$name" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  ip netns exec "$rtr" iptables -A FORWARD -i "up-$name" -j DROP
  # Like any home router, drop what nobody asked for. Accepted, a punch
  # that arrives before our own has left would be tracked as a connection
  # to the router itself, and the port it holds could no longer be ours.
  ip netns exec "$rtr" iptables -A INPUT -i "up-$name" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  ip netns exec "$rtr" iptables -A INPUT -i "up-$name" -j DROP

  if [ "$MOVE" -gt 0 ]; then
    # Linux keeps a port when it can, and when it cannot, picks a new one
    # for every destination. A router that moves ports but keeps the new
    # one for every destination is a fixed rewrite of the port that
    # masquerading kept: made after it on the way out, undone before
    # connection tracking on the way in. XOR is its own inverse.
    ip netns exec "$rtr" nft -f - <<EOF
table ip portmove {
  chain out {
    type filter hook postrouting priority 200;
    oifname "up-$name" udp sport set udp sport ^ $MOVE
    oifname "up-$name" tcp sport set tcp sport ^ $MOVE
  }
  chain in {
    type filter hook prerouting priority raw;
    iifname "up-$name" udp dport set udp dport ^ $MOVE
    iifname "up-$name" tcp dport set tcp dport ^ $MOVE
  }
}
EOF
  fi
done

# ---------------------------------------------------------------------------
# The internet: a STUN server, and a TCP proxy in front of the meeting
# point, standing in for Cloudflare. Through the proxy, every client comes
# from 127.0.0.1 as far as the server can tell.
# ---------------------------------------------------------------------------
cat >"$WORK/internet.py" <<'EOF'
import asyncio, socket, struct

MAGIC = 0x2112A442

class Stun(asyncio.DatagramProtocol):
    def connection_made(self, transport):
        self.transport = transport

    def datagram_received(self, data, addr):
        if len(data) < 20:
            return
        kind, _, cookie = struct.unpack("!HHI", data[:8])
        if kind != 0x0001 or cookie != MAGIC:
            return
        port = addr[1] ^ (MAGIC >> 16)
        ip = struct.unpack("!I", socket.inet_aton(addr[0]))[0] ^ MAGIC
        attr = struct.pack("!HHBBHI", 0x0020, 8, 0, 1, port, ip)
        head = struct.pack("!HHI", 0x0101, len(attr), MAGIC) + data[8:20]
        self.transport.sendto(head + attr, addr)

async def pipe(reader, writer):
    try:
        while data := await reader.read(65536):
            writer.write(data)
            await writer.drain()
    except OSError:
        pass
    finally:
        writer.close()

async def proxy(reader, writer):
    try:
        up_reader, up_writer = await asyncio.open_connection("127.0.0.1", 8080)
    except OSError:
        writer.close()
        return
    await asyncio.gather(pipe(reader, up_writer), pipe(up_reader, writer))

async def main():
    loop = asyncio.get_running_loop()
    await loop.create_datagram_endpoint(Stun, local_addr=("5.0.0.1", 3478))
    await asyncio.start_server(proxy, "5.0.0.1", 443)
    print("ready", flush=True)
    await asyncio.Event().wait()

asyncio.run(main())
EOF
python3 "$WORK/internet.py" >"$WORK/internet.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 50); do
  grep -q ready "$WORK/internet.log" && break
  sleep 0.1
done
grep -q ready "$WORK/internet.log" || { cat "$WORK/internet.log"; echo "STUN and proxy never started" >&2; exit 1; }

log "checking what the routers let through"
check() { # description, netns, target, expectation
  if ip netns exec "$2" ping -c1 -W1 "$3" >/dev/null 2>&1; then got=REACHABLE; else got=BLOCKED; fi
  printf '  %-34s %s' "$1" "$got"
  if [ "$got" != "$4" ]; then printf '   <- expected %s\n' "$4"; exit 1; fi
  printf '\n'
}
check "A -> internet (5.0.0.1)" nsa 5.0.0.1 REACHABLE
check "B -> internet (5.0.0.1)" nsb 5.0.0.1 REACHABLE
check "B -> A's LAN (192.168.1.2)" nsb 192.168.1.2 BLOCKED

stun_port() { # netns, local port -> the port STUN reports
  ip netns exec "$1" python3 - "$2" <<'EOF'
import os, socket, struct, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("0.0.0.0", int(sys.argv[1])))
s.settimeout(2)
s.sendto(struct.pack("!HHI", 1, 0, 0x2112A442) + os.urandom(12), ("5.0.0.1", 3478))
data = s.recv(512)
print(struct.unpack("!H", data[26:28])[0] ^ 0x2112)
EOF
}
seen="$(stun_port nsa 40000)"
printf '  %-34s %s\n' "A's port 40000 seen outside as" "$seen"
[ "$seen" = "$((40000 ^ MOVE))" ] || { echo "the router does not move ports as it should" >&2; exit 1; }

# ---------------------------------------------------------------------------
if [ -n "${FT_BIN_DIR:-}" ]; then
  log "using the binaries in $FT_BIN_DIR"
  cp "$FT_BIN_DIR/puresend-server" "$WORK/server"
  cp "$FT_BIN_DIR/puresend" "$WORK/client"
else
  log "building the binaries (needs every module already in the cache)"
  go build -o "$WORK/server" ./cmd/server
  go build -o "$WORK/client" ./cmd/client
fi

log "starting the server behind the proxy"
"$WORK/server" -ws-port 8080 -health-addr 127.0.0.1:8081 -key "$WORK/server.key" \
  -announce /ip4/5.0.0.1/tcp/443/ws >"$WORK/server.log" 2>&1 &
PIDS+=($!)

for _ in $(seq 1 50); do
  grep -q "Peer ID:" "$WORK/server.log" && break
  sleep 0.2
done
PEER_ID="$(awk '/Peer ID:/ {print $3; exit}' "$WORK/server.log")"
[ -n "$PEER_ID" ] || { cat "$WORK/server.log"; echo "server never started" >&2; exit 1; }
SERVER_ADDR="/ip4/5.0.0.1/tcp/443/ws/p2p/$PEER_ID"
echo "  $SERVER_ADDR"

# ---------------------------------------------------------------------------
log "sending ${SIZE_MB} MB from A to B"
mkdir -p "$WORK/src" "$WORK/out"
head -c "$((SIZE_MB * 1024 * 1024))" /dev/urandom >"$WORK/src/payload.bin"

export GOLOG_LOG_LEVEL="error,p2p-holepunch=debug"
ip netns exec nsa "$WORK/client" -server "$SERVER_ADDR" -stun 5.0.0.1:3478 \
  -send "$WORK/src/payload.bin" >"$WORK/room" 2>"$WORK/send.log" &
SEND_PID=$!
PIDS+=($SEND_PID)

for _ in $(seq 1 100); do
  [ -s "$WORK/room" ] && break
  sleep 0.2
done
ROOM="$(tr -d '\r\n' <"$WORK/room")"
[ -n "$ROOM" ] || { cat "$WORK/send.log"; echo "no room code" >&2; exit 1; }
echo "  room code: $ROOM"

ip netns exec nsb "$WORK/client" -server "$SERVER_ADDR" -stun 5.0.0.1:3478 \
  -receive "$ROOM" -out "$WORK/out" -yes 2>"$WORK/recv.log"
wait $SEND_PID

show_logs() {
  echo "--- the addresses each side offered for hole punching:" >&2
  grep -h 'msg="initiating hole punch"\|msg="received hole punch request"' \
    "$WORK/send.log" "$WORK/recv.log" | grep -o 'addrs="[^"]*"' | sort -u >&2 || true
  echo "--- receiver:" >&2
  grep -v '^ts=' "$WORK/recv.log" >&2 || true
}

log "how did they connect?"
if grep -q "connected directly" "$WORK/recv.log"; then
  got=direct
elif grep -q "fallback relay" "$WORK/recv.log"; then
  got=relay
else
  show_logs
  echo "the receiver never said how it connected" >&2
  exit 1
fi
echo "  $got"
if [ "$got" != "$EXPECT" ]; then
  show_logs
  echo "expected $EXPECT, got $got" >&2
  exit 1
fi

log "checking the bytes"
a="$(sha256sum "$WORK/src/payload.bin" | cut -d' ' -f1)"
b="$(sha256sum "$WORK/out/payload.bin" | cut -d' ' -f1)"
echo "  sent:     $a"
echo "  received: $b"
[ "$a" = "$b" ] || { echo "digest mismatch" >&2; exit 1; }

if [ "$EXPECT" = direct ]; then
  log "and the files must not have gone through the relay"
  relayed="$(wget -qO- http://127.0.0.1:8081/metrics |
    awk '/^libp2p_relaysvc_data_transferred_bytes_total/ {printf "%.0f", $2}')"
  relayed="${relayed:-0}"
  echo "  libp2p_relaysvc_data_transferred_bytes_total: $relayed"
  [ "$relayed" -lt "$((SIZE_MB * 1024 * 1024))" ] ||
    { echo "the relay carried the transfer" >&2; exit 1; }
fi

printf '\n\033[1;32mhole punching through port-moving routers: %s, as expected\033[0m\n' "$got"
