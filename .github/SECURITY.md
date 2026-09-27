# Security Policy

Security and user privacy are foundational to PureSend. Reports and contributions from security researchers and the open-source community are always welcome.

---

## Reporting a Vulnerability

To protect PureSend users, please report security vulnerabilities **privately** rather than opening a public issue or pull request.

The primary and recommended channel is **GitHub Private Vulnerability Reporting**:  
👉 **[Report a Security Vulnerability](https://github.com/Baaaki/PureSend/security/advisories/new)**

This feature works just like submitting an issue, but keeps all reproduction steps, logs, and discussions completely private between you and the repository maintainer until a patch is released.

Alternatively, you can email:  
📧 **<contact@madebybaki.com>**

You do not need a complete or weaponized proof-of-concept to reach out. An early heads-up or rough observation is always welcome.

---

## What to Include

Providing structured information helps validate and remediate findings rapidly:

- **Description:** A clear summary of the issue and its realistic security impact.
- **Reproduction:** Step-by-step instructions, including a proof-of-concept (PoC) script, multi-peer setup, or network capture where applicable.
- **Affected Component(s):** Affected submodules (`cmd/client`, `cmd/server`, `internal/transfer`, `internal/rendezvous`, etc.), release version, commit hash, operating system, and architecture.
- **Preconditions & Environment:** Required execution context (e.g., direct P2P connection vs. Circuit Relay v2 fallback, interactive Bubble Tea TUI vs. headless CLI mode, default public server vs. custom self-hosted rendezvous server, symmetric NAT vs. open firewall).
- **Remediation:** Any proposed code fix, patch, or configuration adjustment if you have developed one.

---

## Safe Harbor

Security research conducted in good faith under this policy is considered **authorized**. For research adhering to these terms:

- No legal action will be pursued against you regarding your research activities.
- You will be publicly credited for valid findings in release notes and GitHub Security Advisories (unless you request anonymity).

**Good-faith research guidelines:**

- **Authorized scope only:** Test exclusively against systems and environments you control — your own client machines, test transfers between your own endpoints, and your own self-hosted rendezvous/relay instances.
- **Protect user privacy & service reliability:** Do not attempt to intercept, eavesdrop on, or alter files belonging to other users. Do not exhaust or degrade shared relay bandwidth or server resources.
- **Zero data exfiltration:** Never retain, copy, or distribute data belonging to others. If incidental data is encountered during testing, halt testing, report the incident immediately, and securely purge all local copies once reported.
- **Coordinated disclosure:** Allow reasonable time for remediation and release before disclosing findings publicly.

If you are uncertain whether a particular testing method falls within scope, reach out to <contact@madebybaki.com> before proceeding.

---

## Scope

### In Scope — PureSend Components

- **PureSend Client (`cmd/client`):**
  - Interactive Terminal User Interface (`internal/tui`) built on Charmbracelet Bubble Tea.
  - Headless and automated transfer CLI modes (`internal/headless`).
- **Transfer Engine & Cryptographic Core (`internal/transfer`):**
  - SPAKE2-style password-authenticated key exchange (`internal/transfer/auth.go`).
  - Session key derivation with mutual libp2p Peer ID binding.
  - Transport encryption between the peers (libp2p Noise / TLS 1.3).
  - Role-separated HMAC confirmation tags (`confirmReceiverLabel`, `confirmSenderLabel`).
  - File manifest parser, path validation, and directory traversal protections (`internal/transfer/names.go`).
  - Destination containment: `safeJoin`, no writing through symbolic links, refusal of the home folder as a destination.
  - Filename sanitization, control character stripping, and bidirectional (Bidi) override mitigation (`internal/safetext`).
  - Windows reserved device name filtering (`CON`, `PRN`, `AUX`, `NUL`, `COM1-9`, `LPT1-9`, etc.).
  - Per-file SHA-256 verification and the resume engine (`.part` staging).
- **Peer-to-Peer & Networking Stack (`internal/p2p`):**
  - libp2p node configuration, multiaddr discovery, and transport negotiation (WebSockets, TLS).
  - Direct Connection Upgrade through Relay (DCUtR) hole punching and STUN traversal.
  - Circuit Relay v2 protocol handling and fallback routing.
- **Rendezvous & Relay Server (`cmd/server`, `internal/rendezvous`):**
  - Ephemeral room registry, nameplate allocation and room lifecycle management.
  - Lookup rate limiting (per-peer lookup budget, server-wide miss pressure).
  - Relay access control lists (ACL), per-reservation bandwidth bounds, and session duration limits.
  - Local diagnostic services: Prometheus metrics (`/metrics`) and health check endpoints (`/health`).
- **Packaging, Deployment & Updates:**
  - Docker container configuration (`Dockerfile`, `docker-compose.yml`).
  - Cloudflare Tunnel routing configuration (`deploy/cloudflared-config.yml`).
  - One-line installation scripts (`install.sh`, `install.ps1`).
  - Binary self-update engine (`internal/update`), release checksums and minisign signatures.

### Out of Scope

- **Volumetric Denial-of-Service (DoS):** Flooding public rendezvous servers, saturating relay network pipes, or resource-exhaustion attacks against public infrastructure without an exploitable application flaw.
- **Social Engineering:** Phishing, spear-phishing, or social engineering targeting the maintainer, server operators, or users.
- **Local Machine / Host Compromise:** Vulnerabilities that require prior physical access, malware execution, or root/administrator privileges on the user's host operating system.
- **Probabilistic Room Code Guessing:** Guessing the secret words of a room code (16 bits) within the expected odds — a few in 65,536 per room — without bypassing the per-room attempt limit or the cryptographic controls.
- **Harmless Content Receipt:** Transferring files containing malware that the receiving user explicitly approved and accepted. (PureSend guarantees data integrity in transit via SHA-256, but does not perform content inspection or antivirus analysis).
- **Unexploited Upstream Dependencies:** Vulnerabilities in third-party Go modules or upstream libraries without a demonstrated, reachable exploit vector within PureSend.
- **Automated Scanner Dumps:** Unvalidated outputs from automated security scanners lacking an actionable proof-of-concept.

---

## Supported Versions

| Version | Supported | Notes |
| :--- | :---: | :--- |
| **Latest Release (v2.x)** | ✅ | Active support. Security patches are prioritized and released promptly. |
| **Pre-release / Beta / Release Candidates** | ⚠️ | Evaluated on a best-effort basis; fixes merge into the upcoming release. |
| **Older Releases (< v2.0.0)** | ❌ | Incompatible protocol. Users must upgrade using `puresend -update` or installer scripts. |

PureSend distributes single static binaries (`CGO_ENABLED=0`). Security fixes are deployed as new tagged releases rather than backported point releases. Self-hosted server operators and end users should keep their installations up to date.

---

## Response & Triage

PureSend is maintained by an independent open-source developer.

- **Triage & Remediation:** Reports are reviewed and addressed on a **best-effort basis** around personal, professional, and military service commitments. Critical vulnerabilities affecting transfer confidentiality, data integrity, or remote code execution are prioritized for resolution.
- **Coordinated Disclosure:** Once a fix is verified, a patched release is published and credited in the GitHub Security Advisory and release notes.

---

## Recognition

With your permission, contributors are credited in:
- The corresponding GitHub Security Advisory.
- The project release notes ([CHANGELOG.md](../docs/CHANGELOG.md)).

PureSend is an open-source, community-driven project and does not currently operate a paid bug-bounty program.

---

## Threat Model & Security Guarantees

Below is a breakdown of what the system protects, against whom, and what falls outside its security boundary. The rendezvous server — and whoever controls the server list clients fall back to — is treated as an adversary for everything except availability and metadata.

### The Room Code

A room code such as `kiraz-liman-42` has two parts with different jobs:

- **The nameplate** (`42`): a number the rendezvous server hands out and finds the room by. It is public. Under load it grows to three or four digits.
- **The secret** (`kiraz-liman`): two words chosen on the sender's machine from a 256-word list, 16 bits. It is **never sent to the server**; it only enters the PAKE handshake between the two peers.

### What PureSend Protects

1. **File Contents Against the Server and the Network:**
   Every byte moves inside a libp2p connection secured with Noise or TLS 1.3 between the two peers — directly after hole punching, or through the Circuit Relay v2 fallback, which forwards ciphertext it cannot read. Which peer is on the other end of that connection is decided by the PAKE handshake over the whole code, with both Peer IDs bound into it:
   - A rogue or compromised rendezvous server — or whoever controls the server list — that names a peer of its own as the sender, or poses as the receiver towards the sender, must know the secret words to complete the handshake. It only ever saw the nameplate, so it has to guess: one guess per attempt, each failure visible to the person it was tried on, and the sender closes the room after 3 of them.
   - A peer in the middle cannot relay the handshake messages between the two honest ends: the Peer IDs bound into the key would differ and confirmation fails.

2. **Online Guessing Is Bounded:**
   Anyone can look up a nameplate and reach the sender, so the secret words are what an attacker has to guess. Each guess costs a full handshake with a live sender. The sender counts handshakes that fail on the code and closes the room after `MaxWrongCodes` (3) of them, telling its user why. Guesses are counted as they are judged, not when their handshakes end, so handshakes held open side by side get no more guesses between them. The odds of a guess landing are therefore at most 3 in 65,536 per room. A handshake abandoned half way is not counted — and gains the guesser nothing, because the sender reveals its own confirmation tag only to a receiver that proved the key first.

3. **Offline Attack Resistance:**
   Nothing derived from the code that could be tested offline crosses the wire: the server receives only the nameplate, and the SPAKE2 messages reveal nothing a listener can test guesses against.

4. **Data Integrity & Atomic Finalization:**
   Every offered file carries a SHA-256 digest in its manifest. The receiver hashes the stream while writing to a staging file (`.part`) and moves it into place **only after** the digest of the whole file matches. An interrupted or corrupted transfer never leaves an incomplete file posing as a finished one. A chunk that would run past the file's declared size is refused as it arrives.

5. **Filesystem Boundary Protection:**
   Manifest data from the remote peer is untrusted input and is checked in full before the user sees the approval screen or anything touches the disk (`internal/transfer/names.go`):
   - Directory traversal (`..`, absolute paths, leading slashes, Windows drive letters `C:\`, backslashes) is rejected, not repaired.
   - Digests must be exactly 64 lowercase hexadecimal characters (they name staging files).
   - Negative sizes, too many files (`maxFiles`) and totals that overflow are rejected.
   - Windows reserved device names and names ending in a dot or space are refused on Windows.
   - The staging folder `.puresend-partial` cannot be targeted, in any letter case.
   - Files are never written through a symbolic link that already sits inside the destination.
   - The destination may not be the home folder itself, a folder above it, or a drive root: there, paths such as `.config/autostart/…`, `.ssh/authorized_keys` or `Library/LaunchAgents/…` would run something or let someone in at the next login.
   - Existing files are never overwritten. A finished download is moved into place with a hard link, which fails if the name is taken, and takes the next free name (`photo (1).jpg`) instead.
   - The approval screen shows what the transfer creates directly in the destination, with hidden (dot) entries listed first and called out, so none can hide at the end of a long list.

6. **Resume Without Revealing the Folder:**
   A resumed transfer skips files that an interrupted earlier attempt already finished. Only files this program finished — marked in `.puresend-partial` — are reported to the sender as present, never arbitrary files that happen to be at the target, so a sender cannot probe the destination for files by their digests.

7. **Terminal & UI Injection Defense:**
   All text from the remote peer or the server (file names, error messages) is sanitized through `internal/safetext` before display. ANSI escape sequences and Unicode bidirectional overrides are stripped, and names containing them are refused.

8. **Transfer Deadlines & State Isolation:**
   Every protocol phase has a deadline (handshake 30 s, approval 5 min, idle 2 min). A room is claimed only by a receiver that completed the handshake, so unauthenticated connections cannot keep the real receiver out.

9. **Release Integrity:**
   `puresend -update` and the install scripts refuse an archive that does not match the release's `checksums.txt`, and refuse a release with no checksums at all. Builds that carry a minisign public key also require `checksums.txt` to be signed with the matching secret key. Releases from 2.0.2 on are signed with `RWQ2F1ZFuTGorH4GqU4qC3PzJo5Evx2OKfNfJSiLbgyoEkMFDwUV8Kts`; check one by hand with `minisign -Vm checksums.txt -P <key>`. Published releases are never replaced: the release workflow refuses to run for a tag that already has one.

---

### What PureSend Does Not Protect

1. **Metadata Confidentiality:**
   The rendezvous server observes when peers connect, their IP addresses, their Peer IDs and which peers meet. File names, sizes and contents stay hidden from it; PureSend is not an anonymity tool.

2. **IP Addresses:**
   A waiting sender registers every address it can be reached on — its public IP and the port its router gave it, as STUN reports them, and its local network addresses, so that two computers on the same network connect directly — and the server hands them to anyone who looks up the nameplate, before any handshake. Nameplates are short and public, so someone who walks them can collect the addresses of the senders waiting at that moment without knowing a single code; the lookup limits slow that down, they do not prevent it. The receiver's addresses reach whichever peer it dials. Once two peers are connected, libp2p's identify protocol tells each the other's listening addresses whether the connection is direct or relayed, so the relay fallback does not hide them either. PureSend is not a tool for hiding your IP address.

3. **Room Codes Shared Over Insecure Channels:**
   The whole code is the authorization for a transfer. Anyone who learns it before the intended receiver uses it can claim the room.

4. **A Lucky Guess:**
   Up to 3 guesses at 65,536 secrets per room is small odds, not zero. A room closed by wrong guesses is a nuisance for its sender (a new code costs one read-out); it is also something a stranger who looks up nameplates can cause deliberately.

5. **Denial of Service:**
   Behind a tunnel every client reaches the server from the same address, and a client mints a new identity on every start, so the server's own limits (one room per peer, a per-peer lookup budget, a server-wide miss budget, dropping abandoned rooms first when the table is full) slow a flood down but cannot stop one. Per-address limits belong at the edge; see [DEPLOYMENT.md](../docs/DEPLOYMENT.md). Room owners hold one connection each, so a determined attacker holding enough connections can still fill the room table.

6. **Sender-Side Selection Mistakes:**
   Selected folders are sent in full, hidden files included. Symbolic links inside them are skipped, not followed.

7. **Safety of Approved Files:**
   A matching SHA-256 proves the file is the one the sender offered. It does not make it safe to open. PureSend does not scan or sandbox transferred files.

8. **Code Signing by the OS Vendor:**
   Binaries are not signed with Apple or Microsoft code-signing certificates, so Gatekeeper and SmartScreen will warn on first launch.

---

## Cryptographic Architecture

PureSend's authentication and key exchange pipeline is implemented in `internal/transfer/auth.go`:

```
Sender (Peer A)                                           Receiver (Peer B)
      │                                                          │
      │ ◄────────── 1. libp2p Connection (Noise / TLS) ────────► │
      │                                                          │
      │ 2. Initialize SPAKE2 Party                               │ 2. Initialize SPAKE2 Party
      │    Password: the whole room code                         │    Password: the whole room code
      │    Role: Sender (Role 1)                                 │    Role: Receiver (Role 0)
      │    Curve: P-256 (Constant-time)                          │    Curve: P-256 (Constant-time)
      │                                                          │
      │ ◄──────────────── 3a. PAKE message (receiver first) ──── │
      │ ───────────────── 3b. PAKE message ────────────────────► │
      │                                                          │
      │ 4. Session key, both Peer IDs bound in                   │ 4. Session key, both Peer IDs bound in
      │                                                          │
      │ ◄──────────────── 5a. Receiver's confirmation tag ────── │
      │ ───────────────── 5b. Sender's tag — only if 5a was ───► │
      │                       right; an empty answer otherwise   │
      │                                                          │
      │ ═══════════ 6. Authenticated Stream Established ═════════│
```

- **PAKE Primitive:** a SPAKE2-style exchange via `github.com/schollz/pake/v3` (not RFC 9382 SPAKE2 or CPace; see the note in `auth.go`) on the NIST P-256 curve (`pakeCurve = "p256"`), backed by the Go standard library.
- **Password:** the whole room code. The rendezvous server only ever sees its nameplate.
- **Identity Binding:** The derived session key binds both the sender and receiver's cryptographic `peer.ID`, preventing cross-session message splicing and man-in-the-middle relay substitution.
- **Role-Separated Confirmation:** Before manifest or file payloads are accepted, both parties exchange HMAC-SHA256 confirmation tags using domain-separated labels. The receiver proves the key first; the sender answers with its own tag only if that proof was right, so no one can test a guess against the sender's tag without it counting as a wrong code:
  - Receiver tag: `confirmReceiverLabel = "puresend/pake/confirm/receiver/v1"`
  - Sender tag: `confirmSenderLabel = "puresend/pake/confirm/sender/v1"`

---

## Server Hardening & Operational Security Guide

For operators running self-hosted rendezvous and relay nodes (`cmd/server`):

1. **Protect the Server Identity Key (`server.key`):**
   - The cryptographic identity of the server defines its libp2p `Peer ID`. Client binaries may pin or discover this server address.
   - Store `server.key` on a secured, non-root readable volume (`chmod 600`).
   - Maintain an offline backup of the key (`base64 -w0 server.key`), restorable via the `FT_IDENTITY_KEY` environment variable.
2. **Isolate Diagnostic & Metrics Ports:**
   - The server exposes `/health` and `/metrics` on port `8081`.
   - Ensure port `8081` binds strictly to `127.0.0.1` and is never exposed to the public internet. Use reverse-proxy authentication or SSH port forwarding for monitoring.
3. **Configure Edge Rate Limiting:**
   - When placing the server behind Cloudflare Tunnel or a reverse proxy, every client appears to come from the proxy's address, and connections from `-trusted-proxies` are exempt from libp2p's per-address limits.
   - Rate-limit new WebSocket connections per client IP at the edge instead. [DEPLOYMENT.md](../docs/DEPLOYMENT.md) has a Cloudflare rule and an Nginx configuration.
4. **Enforce Relay Quotas:**
   - Retain bounded relay constraints: `-relay-data` (default: 256 MB per relayed connection) and `-relay-duration` (default: 10 minutes) keep a failed hole punch from streaming unbounded data through the server.
5. **Enforce Single-Room Concurrency:**
   - Maintain `-rooms-per-peer 1`. A legitimate sender only requires one active room per transfer session. Allowing arbitrary rooms per peer enables state-exhaustion attacks.

---

## Verified Safeguards & Testing

PureSend maintains a comprehensive automated security regression suite in `internal/transfer/hardening_test.go` and `internal/rendezvous/rendezvous_test.go`, verifying:

| Test Case | Defensive Guarantee | Test Verification |
| :--- | :--- | :--- |
| **Server Blindness** | Across a whole transfer, nothing sent to the rendezvous server contains the secret words of the code. | `TestServerNeverSeesTheWords` |
| **Guessing Limit** | The sender closes its room after 3 wrong codes, however the guesses are timed; a receiver that skips its own proof never gets the sender's tag. | `TestTooManyWrongCodesCloseTheRoom`, `TestGuessesHeldOpenTogetherShareTheLimit`, `TestRefusedGuessIsAnsweredAsWrong`, `TestSenderTagNeedsTheReceiversFirst` |
| **Path Traversal Defenses** | Rejects `../`, absolute paths, leading slashes, and Windows drive roots. | `TestUnsafePaths` |
| **Destination Containment** | Refuses the home folder as a destination, writing through links, and replacing existing files. | `TestDestinationIsNotTheHomeFolder`, `TestNoWritingThroughLinks`, `TestPlaceNeverReplaces` |
| **Resume Privacy** | Only files an interrupted transfer finished are reported as present. | `TestResumeRevealsNoOtherFiles` |
| **Release Integrity** | Updates must match `checksums.txt`, and its minisign signature when a key is built in. | `TestUpdateIsVerified`, `TestMinisignVectors` |
| **Manifest Sanitization** | Blocks path-traversal digests, invalid checksum formats, negative file sizes, and arithmetic overflows. | `TestManifestFieldsAreValidated`, `TestManifestTotalCannotOverflow` |
| **Windows Namespace Isolation** | Blocks illegal DOS device names (`CON`, `NUL`, `AUX`, `LPT1-9`, `COM1-9`, trailing dots/spaces). | `TestWindowsNames` |
| **Handshake Impersonation** | Rejects unauthenticated connections, mismatched peer identities, and wrong room codes without leaking secret state. | `TestWrongCodeRejected`, `TestIdentityMismatchRejected` |
| **Denial-of-State & Hijacking** | Prevents unauthenticated receivers from holding or claiming rooms; turns away conflicting claims with `ErrBusy`. | `TestWrongCodeNeverClaims`, `TestSilentReceiverTimesOut`, `TestBusyRoomTurnsAway` |
| **Terminal Control Sanitization** | Strips ANSI escape sequences and Unicode Bidirectional control markers from peer text. | `TestRemoteErrorTextIsCleaned`, `safetext.Clean` |
| **Data Integrity Verification** | Detects transmission bit flips and chunk tampering, terminating transfers without final file rename. | `TestChecksumMismatch` |

---

## Contact & Questions

If you have questions regarding PureSend's security architecture, deployment hardening, or this policy, please reach out via:
- **Email:** <contact@madebybaki.com>
- **Website:** [https://puresend.madebybaki.com](https://puresend.madebybaki.com)
