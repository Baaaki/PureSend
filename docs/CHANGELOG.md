# Changelog

Notable changes to PureSend. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [2.0.5] - 2026-09-27

### Fixed

- **Hole punching works behind routers that do not keep ports.** Many
  home routers give an outgoing socket a port of their own — local port
  60982 goes out as 2332 — and keep it for every destination, which hole
  punching can work with. But a peer told the other side its public IP
  with its *local* port, since STUN was asked from a throwaway socket and
  only its IP was used. The other side punched towards a port nobody was
  on, every attempt failed, and transfers went through the relay. STUN is
  now asked from the QUIC socket itself, and the port the router gave it
  is what goes out for QUIC. The query is repeated every 15 seconds, which
  keeps the router from forgetting the mapping while a sender waits, and
  catches it if the port moves anyway. Each peer offers its own address,
  so every peer behind such a router needs this version.
- Dependabot's Go module runs failed: the Go proxy lists an old
  go-libp2p tag as `v6.0.23+incompatible`, Dependabot took it for the
  newest release, and it cannot be resolved. Versions of go-libp2p from 2
  up are now ignored; all of them are those pre-module tags.

### Added

- `make test-holepunch` (`test/holepunch/netns-holepunch-test.sh`), run by
  CI: two peers behind routers that move ports, built from network
  namespaces, with the meeting point behind a proxy as it is behind
  Cloudflare in production. The receiver must connect directly and the
  relay must not carry the files. 2.0.4 fails it; with routers that keep
  ports, both versions pass.

## [2.0.4] - 2026-09-23

### Security

- `golang.org/x/crypto` 0.54.0 → 0.57.0, which fixes GO-2026-6303,
  GO-2026-6354 and GO-2026-6355. `govulncheck` found none of them
  reachable from PureSend's code; the update takes them out of the build.

### Changed

- `go-libp2p` 0.49.0 → 0.50.0 (quic-go 0.62.0). It carries a hole-punching
  fix: the attempt's timeout is set before the connection notifications
  that can start it.
- **The terminal interface runs on Bubble Tea v2, Lip Gloss v2 and
  Bubbles v2.** The screens and keys are the same. Colours now follow the
  background the terminal reports when asked, and the code box keeps the
  terminal's own text colour for its prompt and cursor, which the new
  defaults would have drawn in a grey that is hard to see on a light
  background. A pasted code still lands in the code box: v2 delivers a
  paste as one message rather than as keys, and it is routed there.
- `pion/stun` 3.1.7 → 4.0.1.
- Go 1.27.1: go.mod names it as the toolchain, so a machine with an older
  1.27 fetches it, and the server image builds on `golang:1.27.1-alpine3.24`
  rather than whatever `1.27-alpine` a builder has cached. The server
  image runs on Alpine 3.24 instead of 3.22.
- The rest of the dependency tree is current as of September 2026, except
  libp2p's own network stack (quic-go, webtransport-go, the pion WebRTC
  packages), which stays at the versions go-libp2p 0.50.0 is built and
  tested against; newer pion releases do not even compile together.
- CI and the release job run on `ubuntu-26.04`.
- The release page's install notes and the `.deb` package description are
  in English.
- Dependabot groups the `charm.land` modules with the rest of Charm.

### Fixed

- Something a library logged — quic-go's warning about UDP buffer sizes,
  on most Linux machines — was written over the terminal interface, and
  the new renderer, which redraws only the cells it changed, would have
  left it there. Log output is now kept off the screen while the
  interface runs.
- `make deb` stamped every package it built as 2.0.1, whatever the code
  was. It now takes the version from the latest release tag, and builds
  in the release signing key, so a locally built package checks update
  signatures the way the released one does.

## [2.0.3] - 2026-09-23

Nothing in the program changed; the binaries differ from 2.0.2 only in
their version stamp. This release is the first one built by the updated
release workflow below.

### Fixed

- The AUR `PKGBUILD` was still at 2.0.0 after 2.0.1 went out; it now
  follows each release, with that release's archive digest.

### Changed

- CI and the release workflow run on the Node 24 versions of their
  actions (checkout, setup-go, upload-artifact, goreleaser-action)
  instead of the Node 20 ones GitHub has deprecated, and on
  `ubuntu-24.04` rather than `ubuntu-latest`, which is about to move to
  a new Ubuntu under every job at once.

## [2.0.2] - 2026-09-23

### Security

- **A room allows 3 guesses at its code, however they are timed.** Wrong
  codes were counted when a handshake ended, so a guesser who held several
  handshakes open at once, each waiting only on its verdict, had every one
  of them judged before the count caught up: 4 guesses instead of 3, and
  the fourth, if right, got the files. Each proof is now counted as it is
  judged, and once a room has had its 3 wrong codes, no further proof is
  judged at all. Only guesses that are judged count; a handshake that
  breaks off before its proof, or sends a malformed message, tells the
  guesser nothing and uses up nothing.
- **Releases are signed.** `checksums.txt` now comes with a minisign
  signature. Clients built from this release on refuse an update whose
  checksum list is not signed with the project's key, and the install
  scripts carry the same key. The public key is in `SECURITY.md`.

### Added

- `docs/DEPLOYMENT.md` lists every secret the project holds — where each
  one lives, what losing or leaking it costs, how to store it and how to
  replace it — and the order a release goes out in: the tag first, `main`
  once the signed release is live, and the website's downloads taken from
  the release itself rather than built again.

## [2.0.1] - 2026-09-23

### Security

- **A refused transfer no longer tells the sender where the receiver saves
  files.** A failure to write to disk was passed on word for word, local
  path included — and with it, on most machines, the account name. The
  sender now hears the reason ("no space left on device"), not the path.
- **The server never replaces an identity key it cannot read.** Any read
  error other than a missing file used to mean "generate a new key", which
  on a file that could still be written changed the peer ID and stranded
  every client already released. It is now a reason to stop, and a new key
  is only ever written to a file that does not exist yet.
- `install.ps1` checks the release signature when minisign is installed,
  as `install.sh` already did. Neither does until signing is set up
  (`docs/DEPLOYMENT.md`).

### Fixed

- **The security policy no longer says the relay hides IP addresses.** A
  waiting sender's public and local addresses go to anyone who looks up its
  nameplate, and libp2p's identify protocol shares them over a relayed
  connection too. `SECURITY.md` now says so.
- **Release notes come from this changelog.** `.goreleaser.yaml` carried
  the 2.0.0 notes as a fixed header that every later release would have
  repeated, with claims the code does not bear out: that the server's
  ability to impersonate a peer was gone entirely (it is down to 3 guesses
  in 65,536 per room), that the room table has no cap (it has,
  `-max-rooms`), and that `install.ps1` checks signatures. The release
  workflow now publishes the version's section of this file and refuses a
  tag that has none.
- **The release workflow checks the server address it bakes in.** The step
  named "Check the server address is configured" only printed it. It now
  refuses to build when the address is missing from the published
  `server.txt`, which is how a default left behind after a key change
  would show. The tests in that job no longer see the production address.
- The installers no longer fall back to a hard-coded `v2.0.0` when the
  GitHub API does not answer — the next release would have quietly
  installed an old one. They ask the `/releases/latest` redirect instead,
  and stop if that fails too.
- `puresend -update` speaks the user's language instead of always Turkish,
  and leaves a copy installed by the `.deb` or the AUR package to that
  package manager instead of replacing a file it owns.
- The AUR `PKGBUILD` downloads the versioned release archive from GitHub,
  checked against the same digest as `checksums.txt`, instead of an
  unversioned binary on the website that was still 1.0.0.
- A sender connected through the relay is no longer told the relay's limit
  is "0 B"; only the receiver learns the limit.
- The 2.0.0 notes below said room scaling removed the cap on concurrent
  rooms; it did not.
- **Notices follow the language switch.** The wrong-code warning, the
  "attempt was interrupted" notice and the update banner were stored as
  finished sentences, so a sender who pressed `[L]` kept reading them in the
  old language. They are now kept as facts and put into words when drawn;
  the code entry placeholder follows the switch from any screen too.
- **Performance documentation now reports measurements only.** The README
  claimed "100% bandwidth", a 2.5G line rate that was never measured, and
  called the handshake "zero-knowledge"; `BENCHMARK.md` listed a 0-allocation
  result for `safetext.Clean` that the code has never had (it is ~650 ns and
  4 allocations). Both now carry measured numbers with the command behind
  each, and say what has not been measured.
- The handshake is described as what it is — a SPAKE2-style exchange from
  `schollz/pake`, not RFC 9382 SPAKE2 — everywhere, as `auth.go` already did.
- The AUR package no longer describes PureSend as WebRTC.

### Added

- `make bench-e2e` (`scripts/bench-e2e.sh`): throughput and peak memory of
  the real binaries end to end on loopback. On a Ryzen 7 5700X: 360–390 MB/s,
  35–41 MB of RAM from 256 MiB up to 8 GiB.
- `BenchmarkHandshake`: the room-code handshake takes ~0.6 ms of CPU.
- `make cover` counts the client binary the integration tests start, so
  flag handling and headless mode show up in coverage (72% → 77% overall).
  CI reports the same number.
- A test that every message exists in both languages.
- The folder picker warns immediately on screen when [s] is pressed on a forbidden destination (such as the home folder itself or drive root), instead of waiting for a transfer to fail.

### Changed

- The terminal interface is split by concern (`tui.go`, `update.go`,
  `view.go`, `format.go`), and its key handling into one function per screen.
- Receiver status steps are shared constants of `p2p`, not strings the
  interface had to spell the same way.

### Removed

- The room code screen and its messages, unused since the sender's waiting
  screen took its place.

## [2.0.0] - 2026-09-22

This release works through a security review of 1.0.0. The rendezvous
protocol changes incompatibly (`/puresend/rendezvous/2.0.0`): clients and
servers from 1.0.0 cannot talk to this version.

### Security

- **The server no longer sees the room code.** 1.0.0 claimed the rendezvous
  server did not need to be trusted, but it received every code in full
  and could therefore run the handshake with both ends and read or swap
  any transfer. A code is now two secret words chosen by the sender and a
  nameplate — the number — handed out by the server. Only the nameplate is
  ever sent to it; the words go into the PAKE handshake alone. The same
  holds for whoever controls the server list.
- **Wrong codes close the room.** A sender closes its room after 3
  handshakes that fail on the code and tells its user why; each attempt is
  shown as it happens, with how many are left. Nameplates are public, so
  this is what bounds guessing: at most 3 in 65,536 per room.
- **The sender's confirmation tag is only sent to a receiver that proved
  the key first.** A guesser that sent garbage instead of its own tag used
  to receive the sender's anyway — enough to check a guess offline without
  it ever counting as a wrong code.
- **No more server-wide budget of new rooms per minute** (`-register-budget`
  is gone). Identities cost nothing, so spending the budget told every real
  sender "server is busy". A full table now makes space by dropping rooms
  whose owner has disconnected. Per-address limits belong at the edge;
  `docs/DEPLOYMENT.md` now shows how to set them up on Cloudflare and Nginx.
- **Updates and installs are verified.** `puresend -update`, `install.sh`
  and `install.ps1` refuse an archive that does not match the release's
  `checksums.txt`, or a release without one. Releases can be signed with
  minisign; builds that carry the public key (`FT_UPDATE_KEY`) require the
  signature. On Windows a failed replacement puts the old binary back.
- **Releases are immutable.** The release workflow no longer deletes and
  re-creates an existing release; it refuses to run for a tag that already
  has one. Its actions are pinned to commit SHAs and GoReleaser to an exact
  version.
- **Receiving into the home folder is refused**, as is any folder above it
  or a drive root: a sender decides the paths inside a transfer, and there
  `.config/autostart/…` or `.ssh/authorized_keys` would take effect at the
  next login.
- **The approval screen can no longer hide an entry.** A long file list is
  shown as what lands directly in the destination, hidden entries first and
  called out, instead of the first twelve files.
- **No writing through symbolic links** already inside the destination, and
  finished files are moved into place with a hard link, so a file that
  appears at the target between the check and the move is never replaced.
- **The reserved `.puresend-partial` folder is matched in any case**, as the
  case-insensitive filesystems of macOS and Windows see it.
- **Resume no longer reveals what the destination holds.** Only files an
  interrupted transfer finished are reported to the sender as present.
- **Chunks that run past a file's declared size are refused as they
  arrive**, and a sender never sends more of a file than its manifest said.

### Changed

- **Dynamic room scaling.** Room nameplates start at two digits and move
  to longer numbers as the table fills, so codes stay short under light
  load. The table itself is still capped (`-max-rooms`, 1000 by default).

### Fixed

- Documentation: compression is DEFLATE (`flate.HuffmanOnly`), not Snappy;
  rooms live for an hour, not ten minutes; SHA-256 is per file, not per
  chunk; the relay defaults are 256 MB and 10 minutes; the installers now
  suggest `puresend -send` / `-receive`, and the ARM64 Linux hint builds
  from a clone instead of a `go install` path that never existed.
- `FindAsset` no longer takes a `darwin` archive for Windows ("dar**win**").
- Removed the unused `IsProbablyCompressible`.

## [1.0.0] - 2026-09-21

This release works through the findings of the initial end-to-end test report
([`docs/TEST-RAPORU.md`](https://github.com/Baaaki/PureSend/blob/ca8ddc9/docs/TEST-RAPORU.md),
since removed from the tree) — every issue it raised and every
improvement it suggested — and then through a pre-release security and
operations review of the result.

### Security

- **The room code is now a password, not just a lookup key.** Both ends run
  a password-authenticated key exchange (PAKE over P-256) over the code and
  prove the derived key to each other before a manifest is sent. The code
  never crosses the wire, a wrong one fails before the file list is
  revealed, and guessing costs a full connection each time — there is no
  offline attack to speed the search up.
- **The rendezvous server is no longer a trusted party.** It is the server
  that tells the receiver who the sender is; if it names a peer of its own,
  that peer cannot complete the handshake. Both peer IDs are bound into the
  exchange, so a peer in the middle cannot relay the two ends' messages to
  each other either.
- **Room codes are single-use.** When a transfer completes, the sender drops
  the transfer protocol handler and unregisters the room. Previously the
  room stayed live until the user pressed a key, so anyone else who had been
  told the code could download the files again — silently, with the sender's
  screen still saying "sent". A *failed* attempt still leaves the code
  usable, so a flaky link does not cost a phone call.
- **Rooms are released as soon as they are done with**, rather than sitting
  in the server's table until their one-hour expiry.
- **Keeping the room table for real users.** One room per sender
  (`-rooms-per-peer`, default 1 — the program never opens more), a
  server-wide budget of new rooms per minute (`-register-budget`), room
  codes held to their exact shape (a room name used to be up to 16 KB of
  anything), and a room whose owner disconnects is dropped after a minute
  instead of holding its slot for the rest of the hour.
- **A server-wide limit on failed lookups** closes the hole in the per-peer
  one: a client mints a fresh identity on every start, so an attacker
  willing to reconnect could reset its own counter. It does not refuse
  everyone once spent — a first draft did, which let a few random guesses a
  second lock every real user out. Under pressure a peer gets one guess;
  someone typing the right code first time is never refused. Every refusal
  is decided before the room is looked at, so it never reveals whether a
  guess hit.
- **Manifests are validated in full before anything is used.** The digest
  names the partial download a resume continues from, and was not checked:
  a sender could put a path in it and have the receiver create, truncate
  or delete a `.part` file outside the target folder. Digests must now be
  64 lowercase hex characters, sizes non-negative, totals must not
  overflow.
- **File names with control characters or bidirectional marks are
  refused.** They were printed on the approval screen, where an escape
  sequence could repaint the list being approved. On Windows, names
  Windows cannot store (`? * < > | "`, `CON`, `NUL`, a trailing dot) are
  refused before the transfer instead of failing at the final rename.
- **Text from the other side is cleaned before it is shown.** Error
  messages from the other peer and the server could carry escape
  sequences into the terminal.
- **Every wait has an end.** The handshake has 30 seconds, the answer to
  the file list five minutes, a stalled transfer two minutes. And the room
  is claimed only *after* the handshake: someone who connected and went
  quiet used to hold the sender's one slot indefinitely, turning the real
  receiver away with a reset connection.
- **The health and metrics endpoint listens on 127.0.0.1** unless told
  otherwise (`-health-addr`, replacing `-health-port`). A binary started
  by hand used to publish `/metrics` to the whole LAN.
- **Dependencies with reachable vulnerabilities upgraded**: go-libp2p
  v0.49.0 (quic-go v0.60.0, webtransport-go v0.11.1) and pion/dtls v3.1.4.
  `govulncheck` now reports none the code can reach.
- **Manifest paths are validated before anything is written**, and anything
  that could escape the target folder is refused outright rather than
  quietly repaired.
- The server image now runs as a non-root user.

### Added

- **The sender survives the meeting point going away.** A cloudflared
  restart or a redeploy used to take the relay slot and the room with it
  while the sender sat on "waiting" forever and its friend was told the
  code did not exist. The sender now notices, says so on screen,
  reconnects with backoff and puts the same code back — for as long as the
  code's hour lasts.
- **A server list for when the server moves.** Released clients carry the
  address of a small text file (the landing page's `server.txt`, or
  `-server-list` / `FT_SERVER_LIST`) that they read only when none of their
  built-in addresses answers. It is the insurance for every copy already
  downloaded if the server's address or identity ever changes.
- **Files are read while the code is on screen.** The sender used to
  compute every digest after the receiver connected — on every attempt —
  while the receiver stared at "preparing". Now it starts the moment the
  code is shown, re-reads only files that changed since, and keeps an
  early receiver informed of its progress.
- **A clear "busy".** A second receiver with the right code, arriving while
  a transfer runs, is told the room is taken instead of getting a reset
  connection.
- **Room codes in Turkish.** 256 plain-letter Turkish words, none one letter
  from another or the start of another, so "kiraz-liman-42" is what the
  code looks like — as the interface always claimed. 5.9 million codes,
  up from 1.7 million.
- **Codes are forgiving to type.** Case, Turkish letters and spaces are
  normalized; a malformed code or an unknown word is pointed out on the
  code screen, before the server is asked and a miss is counted.
- **libp2p's own metrics on `/metrics`** — the relay service, connection
  limits and transports. They were recorded all along, into a registry
  nobody served. `libp2p_relaysvc_data_transferred_bytes_total` answers
  how much traffic falls back to the relay; a comment used to promise a
  `puresend_relayed_transfers` metric that never existed. New
  counters for abandoned rooms and throttled registrations.
- **A `HEALTHCHECK` in the image**, and a hardened compose file:
  read-only root filesystem, all capabilities dropped,
  `no-new-privileges`, memory and process limits, log rotation.
- **`make vuln`**, and a vulnerability check in CI.
- **A CI workflow for the landing page** (lint, build, check that
  `server.txt` ships), with an optional Vercel deploy job.

- **Folder transfer.** A chosen folder is walked and its structure rebuilt
  on the other side. Symbolic links are skipped rather than followed.
- **Resume.** An interrupted download is kept, keyed by the digest of the
  file it belongs to, and the next attempt continues from where it stopped
  instead of starting over. Abandoned partials are swept after a week.
- **Transfer speed and remaining time**, smoothed over a short window so the
  numbers are steady enough to read, and correct when a transfer resumes
  partway through.
- **A choosable download folder** — from the main menu, from the code-entry
  screen with `Ctrl+O`, or with `-out`.
- **Headless mode**: `-send`, `-receive`, `-yes`. The room code goes to
  stdout on its own line so a script can read it. This is also what lets the
  end-to-end tests drive the real binary instead of typing into a
  pseudo-terminal and scraping ANSI output.
- **`-version`** on both binaries, stamped at build time, and the server's
  version in `/health`.
- **Prometheus metrics** at `/metrics`: active rooms, rooms opened, closed
  and expired, lookups found, missed and throttled, plus the Go runtime and
  process collectors.
- **Several meeting point addresses** may be given, comma-separated, and are
  tried in order — insurance against the single address baked into every
  released client going away.
- **A relay size warning.** When the direct route could not be opened and
  the transfer is larger than the relay will carry, the receiver is told
  before it starts rather than after it breaks.
- **A CI job for the relay fallback.** Two peers in isolated network
  namespaces that can each reach the server but never each other, so hole
  punching is impossible and the relay path *must* work. It needs no
  privileges, so it runs on every commit.
- Linting (`golangci-lint`), race-detector test runs, coverage reporting,
  and a Docker image smoke test in CI. A `Makefile` for all of it.

### Changed

- **The server is sized for running behind a tunnel.** libp2p allows an
  address 8 connections, 8 relay reservations and a trickle of new
  connections, and exempts only loopback. Behind cloudflared — and through
  Docker's bridge, which is not loopback — every client shares one
  address, so the ninth concurrent sender was refused a relay slot and the
  ninth connection refused outright. Connections from `-trusted-proxies`
  are no longer limited per address, the relay holds a reservation for
  every room the server can hold, and the server-wide connection ceiling
  is raised to match.
- **The headless sender keeps the room open after a failed attempt**, as
  the interface does. It used to exit, taking the code with it.
- **A retried transfer does not fetch finished files again.** They used to
  arrive a second time as `name (1).ext`.
- **One Go version throughout**: `go.mod` (now `go 1.26`; Go 1.25 is out of
  support), the Dockerfile, CI and the release build.
- **`make lint` works on any Go.** The linters run through `go run` at the
  versions CI pins; a `golangci-lint` built by an older Go fails on a newer
  standard library with errors that look like the project's.

- **The wait for a direct connection adapts.** It used to be a flat 20
  seconds whatever happened; now DCUtR's own report that it has given up
  ends the wait early, which leaves room for a longer backstop (30s) for the
  slow mobile link where hole punching does eventually succeed.
- **Progress carries the whole transfer's totals**, not just the current
  file's, so a folder of a thousand files shows one honest bar.
- **The health endpoint is fatal when its port is taken.** It used to log a
  line and carry on, leaving a server that looked alive to its supervisor
  and dead to its health check, forever.
- **In the file picker**, backspace goes back up a folder again; `x` removes
  the last pick. Intercepting backspace left the user stuck in a directory
  as soon as they had chosen anything.
- Protocol versions: `/puresend/rendezvous/1.1.0` (adds
  `unregister` and the relay limit in a lookup response) and
  `/puresend/transfer/2.0.0` (adds the handshake, folder paths and
  resume). No compatibility is kept with the earlier versions; none were
  ever released.

### Fixed

- **Resuming a folder that holds the same file twice silently corrupted
  the second copy.** Both copies shared one partial download; the first
  finished and took it away, and the second continued "from where it
  stopped" in a fresh file padded with zeroes — and passed its checksum,
  because the digest state had been computed from the old leftovers. Each
  copy now has a partial of its own.
- **The first release would have failed**: GoReleaser needs `syft` for the
  SBOMs and the release workflow did not install it. It also published
  without running a single test; now it runs vet and the whole suite
  first.
- **The relay CI job depended on a warm module cache**: it built the
  binaries inside a network namespace with no network. They are built
  before entering it now, and the job lifts Ubuntu 24.04's AppArmor
  restriction on unprivileged user namespaces where it applies. The test
  also checks the relay's metrics saw the transfer.
- `.dockerignore` now excludes the landing page and `node_modules`, which
  bloated every build context after an `npm install`.

- **Technical error text no longer reaches the screen.** A dropped
  connection used to surface as `stream reset: stream reset: connection
  closed: unexp…`, against the project's own rule. `explain()` now covers
  dropped connections, mismatched codes, throttling, unsafe names, full
  servers, unreadable files and a full disk — each with something the user
  can actually do about it.
- The Prometheus registration would have panicked at startup: the Go
  collector already publishes a `version` label on `go_info`, and wrapping
  it with a second one is fatal. Caught by the new relay test.
