# PureSend — Architecture and Protocol Flow

[Türkçe](ARCHITECTURE_TR.md)

This document describes the network topology, the cryptography and the algorithm flow of PureSend's peer-to-peer (P2P) file transfer, with diagrams.

---

## 1. System and Network Topology

PureSend moves files straight from device to device, end to end encrypted, and stores nothing on a central server. The rendezvous server only introduces the two peers: it cannot see the files and never learns the secret words of a room code.

```mermaid
flowchart TB
    subgraph Users["Users"]
        SenderUser["Sender (User A)"]
        ReceiverUser["Receiver (User B)"]
    end

    subgraph PureSendSystem["PureSend CLI"]
        SenderApp["Sender node (Client A)"]
        ReceiverApp["Receiver node (Client B)"]
    end

    subgraph Infrastructure["Signaling infrastructure"]
        CFEdge["Cloudflare Edge (WSS / 443)"]
        RendezvousServer["Rendezvous & Relay server (cmd/server:8080)"]
    end

    SenderUser -->|"Picks files, reads out the room code"| SenderApp
    ReceiverUser -->|"Enters the room code & approves"| ReceiverApp

    SenderApp <-->|"1. Signaling & room registration (WSS)"| CFEdge
    ReceiverApp <-->|"1. Room lookup (WSS)"| CFEdge
    CFEdge <-->|"ws://localhost:8080"| RendezvousServer

    SenderApp <-.->|"2. Direct P2P tunnel (DCUtR / TCP / QUIC)"| ReceiverApp
    SenderApp <-.->|"3. Fallback: Circuit Relay v2 (only if no hole can be punched)"| RendezvousServer
    ReceiverApp <-.->|"3. Fallback: Circuit Relay v2 (only if no hole can be punched)"| RendezvousServer
```

---

## 2. End-to-End Algorithm and Protocol Flow (Sequence Diagram)

The four stages of the PureSend protocol: signaling, NAT traversal, PAKE authentication, streaming.

```mermaid
sequenceDiagram
    autonumber
    participant S as Sender
    participant Rnd as Rendezvous Server
    participant R as Receiver

    Note over S,Rnd: 1. Room matching (Discovery)
    S->>Rnd: WSS connection & room registration (addresses only, PeerID_S)
    Rnd-->>S: Room number "42" (TTL: one hour at most)
    Note over S: Picks the secret words itself → code "kiraz-liman-42"
    S-->>R: Code is passed on by voice or text (outside the server)
    R->>Rnd: WSS connection & room lookup (only "42")
    Rnd-->>R: Sender addresses (PeerID_S, multiaddrs)

    Note over S,R: 2. NAT traversal (Hole Punching) with DCUtR
    Note over S,R: Each peer first learns the public address its router gave its QUIC socket (STUN, refreshed every 15 s) and offers that
    R->>Rnd: Open a bridge to the sender through the relay
    Rnd->>S: Forward the bridged connection
    S->>R: DCUtR port-mapping synchronization
    S-->>R: Direct P2P socket opened (server out of the data path!)

    Note over S,R: 3. Authentication with the whole code (PAKE)
    R->>S: PAKE message 1 (P-256, password: the whole code)
    S->>R: PAKE message 2
    Note over S,R: Shared key derived (both Peer IDs bound to the session)
    R->>S: ConfirmReceiver (HMAC-SHA256)
    S->>R: ConfirmSender — only if the receiver's tag was right
    Note over S: The room closes after 3 wrong codes

    Note over S,R: 4. Manifest, approval and streaming
    S->>R: Offer / Manifest (file list, sizes, SHA-256)
    Note over R: Manifest validated, free disk space checked, the user approves the file list (nothing is written before this)
    R-->>S: Transfer Ack: Accepted (resume offsets)

    loop For every 32 KB chunk
        S->>R: 32 KB chunk (DEFLATE HuffmanOnly if it shrinks)
        R->>R: Write to disk (.part) & add to the file's SHA-256
    end
    Note over R: When a file ends its SHA-256 is compared, and on a match it moves to its final name

    R->>S: Final Ack (transfer succeeded)
```

---

## 3. Transport and Fallback Decision Tree (Traversal Flowchart)

How the route is chosen at runtime, depending on the connection:

```mermaid
flowchart TD
    Start(["Transfer started"]) --> DirectLAN{"On the same local network (LAN)?"}

    DirectLAN -- "Yes" --> UseLAN["Direct LAN socket (speed: the link's line rate)"]
    DirectLAN -- "No" --> HolePunch{"DCUtR hole punching succeeded?<br/>(cone NAT / port mapping)"}

    HolePunch -- "Yes (default)" --> UseDirectWAN["Direct WAN P2P tunnel<br/>(data never touches the server, line-rate limited)"]
    HolePunch -- "No (symmetric NAT)" --> RelayFallback["Circuit Relay v2 bridge<br/>(encrypted fallback, speed and quota limited)"]

    UseLAN --> StartCrypto["PAKE (P-256) handshake"]
    UseDirectWAN --> StartCrypto
    RelayFallback --> StartCrypto

    StartCrypto --> VerifyAuth{"Password / code matched?"}
    VerifyAuth -- "Yes" --> TransferStream["32 KB chunk stream + DEFLATE + per-file SHA-256"]
    VerifyAuth -- "No" --> DropConn["Reject — the room closes on the 3rd wrong code"]
```

---

## 4. Core Algorithm Principles

1. **An Untrusted Rendezvous Server:**
   * The handshake is the SPAKE2-style exchange of the `schollz/pake` library; it is not RFC 9382 SPAKE2 or CPace. The parts that carry the security (binding both identities to the key and the mutual confirmation tags) are the project's own code; the rationale is in `internal/transfer/auth.go`.
   * A room code has two parts. In `kiraz-liman-42`, `42` is the public room number (nameplate) that the server hands out; `kiraz-liman` is the secret part the sender chose itself (16 bits) and is never sent to the server.
   * The server cannot see file contents or file names. To put its own node in the sender's place, or to act as the receiver, it would have to guess the secret words: every attempt is a handshake, every failure is visible, and the sender closes the room after 3 wrong codes.
   * During the PAKE handshake the clients bind both Peer IDs into the key, so a peer carrying messages in between cannot join the two ends to each other.
2. **Constant-Memory Streaming ($O(1)$ RAM):**
   * Files are never loaded into memory. They are read in fixed 32 KB chunks, compressed with DEFLATE (`flate.HuffmanOnly`) if that makes them smaller, and sent as they are if it does not.
   * On the receiving side every chunk is added to the file's SHA-256 as it is written to disk; the digest is compared once, when the file ends.
3. **Resume:**
   * When a connection drops, the receiver tells the sender the size of the `.part` file in `.puresend-partial`; the transfer continues from the missing byte only.
   * The "I already have this file" answer is given only for files that an interrupted PureSend transfer finished; a sender cannot use it to probe the destination folder for other files.
4. **Filesystem Safety:**
   * Every path in an incoming manifest goes through the `safeJoin` check. Absolute paths (`/etc/passwd`), directory escapes (`../`), the reserved `.puresend-partial` folder and Windows device names (`CON`, `PRN`, `AUX`) are not repaired, they are rejected.
   * The destination cannot be the home folder itself or a folder above it; nothing is written through symbolic links in the destination; no existing file is overwritten.
