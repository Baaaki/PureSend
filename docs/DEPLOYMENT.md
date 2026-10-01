# PureSend — Server Setup and Deployment Guide

[Türkçe](DEPLOYMENT_TR.md)

This guide covers what it takes to run the PureSend rendezvous and relay server securely, without interruption and in a standard way, with Docker Compose or behind a reverse proxy.

---

## 1. Architecture and Prerequisites

The server (`cmd/server`) never touches the clients' file contents and sees only the number of a room code (`42` for `kiraz-liman-42`); the secret words are never sent to it:
* **8080/TCP:** libp2p WebSocket signaling and DCUtR hole-punching port.
* **8081/TCP:** health check (`/health`) and Prometheus metrics (`/metrics`) port (local access only).

```
Client ──wss://p2p.example.com:443──► [Reverse proxy / Tunnel] ──ws://localhost:8080──► PureSend server
                                         (TLS termination)                                (Docker)
```

---

## 2. Quick Start: Docker Compose (Recommended)

The easiest and safest way is to use the `docker-compose.yml` at the root of the repository.

### Step 1: Set Your Domain and Start

```bash
# Define your domain as an environment variable and start the server
PUBLIC_HOST=p2p.example.com docker compose up -d
```

*(Alternatively, create a `.env` file at the repository root containing `PUBLIC_HOST=p2p.example.com`.)*

> ⚠️ If `PUBLIC_HOST` is not set, `docker-compose.yml` falls back to the project's own hostname (`rendezvous.madebybaki.com`) and your server will announce that address to clients. Always set it for your own deployment.

### Step 2: Get the Server's Peer ID

The first time the server starts it generates a persistent identity key. Clients need the resulting Peer ID to connect to the server:

```bash
docker compose logs rendezvous | grep "Peer ID"
```

Example output of the full startup banner:
```text
Rendezvous + relay server 2.0.7 is running.

  Peer ID: 12D3KooWKKqpYTw3D8arNmcNG7ZK1mPfSH2cQ7ohZqHBmYN6eEAn
  Identity from: /data/server.key

Client address (bake this into the client build):
  /dns4/p2p.example.com/tcp/443/tls/ws/p2p/12D3KooWKKqpYTw3D8arNmcNG7ZK1mPfSH2cQ7ohZqHBmYN6eEAn
```

> ⚠️ **IMPORTANT:** The `rendezvous-key` Docker volume holds the server's identity. If it is deleted, the Peer ID changes and existing clients can no longer connect to the server.

---

## 3. Reverse Proxy and Tunnel Options

The server listens on plain `ws://` inside the local network, so TLS has to be terminated by a reverse proxy. Pick the option that fits you:

### Option A: Cloudflare Tunnel (No Static IP Required)

If your server has no fixed public IP or open port, Cloudflare Tunnel is the most practical solution.

Add this to your `cloudflared` ingress configuration (`/etc/cloudflared/config.yml`):

```yaml
tunnel: <tunnel-uuid-or-name>
credentials-file: /root/.cloudflared/<tunnel-uuid>.json

ingress:
  - hostname: p2p.example.com
    service: http://localhost:8080
    originRequest:
      connectTimeout: 30s
  - service: http_status:404
```

Restart the service:
```bash
sudo systemctl restart cloudflared
```

> If the machine already runs a tunnel for something else, merge the `hostname` block into the existing `ingress:` list instead of replacing the file. [deploy/cloudflared-config.yml](../deploy/cloudflared-config.yml) is an annotated example that explains how.

---

### Option B: Caddy (Automatic Let's Encrypt TLS)

If you use a VPS with a fixed IP, Caddy obtains the SSL certificate automatically and forwards the WebSocket traffic.

`/etc/caddy/Caddyfile`:
```caddy
p2p.example.com {
    reverse_proxy localhost:8080
}
```

---

### Option C: Nginx

If you already have an Nginx setup, use `/etc/nginx/sites-available/puresend.conf`:

```nginx
server {
    server_name p2p.example.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_read_timeout 86400s;
        proxy_send_timeout 86400s;
    }

    listen 443 ssl; # add your SSL certificate directives
}
```

---

### For All Options: Per-IP Rate Limiting at the Edge

Behind a tunnel or reverse proxy, every client reaches the server from the same address (the tunnel itself). That is why connections from `-trusted-proxies` networks are exempt from libp2p's per-address limits; without the exemption, every user in the world would share one address's allowance of 8 connections. The server's own limits (one room per peer, 5 lookups per minute per peer, 200 empty lookups per minute server-wide) make each new connection a little more expensive, but because a client identity is free they **cannot stop** a flood from a single address. Room numbers are public and a sender closes its room after 3 wrong codes, so anyone who can open unlimited connections can walk the numbers and close rooms. The per-address limit belongs at the edge.

Every client session is a single WebSocket connection, so what we count is new connection requests to the `/` path.

#### Cloudflare (with Tunnel)

Cloudflare Dashboard → your domain → **Security** → **WAF** → **Rate limiting rules** → **Create rule**:

| Setting | Value |
| :--- | :--- |
| Rule name | `PureSend connection limit` |
| Expression | *URI Path* **equals** `/` (in the expression editor: `(http.request.uri.path eq "/")`) |
| Counting characteristic | IP |
| Limit | **5 requests per 10 seconds** |
| Action | **Block**, duration **10 seconds** |

The free plan allows a single rule; only the path field can be used in the expression, and the counting and blocking periods are 10 seconds. Because the host name cannot be selected, the rule applies to the `/` requests of **all** subdomains that pass through Cloudflare in this zone; if the same zone also hosts a website, requests to its home page are counted too. 5 requests in 10 seconds does not get in a human's way. On Pro and higher plans you can add `http.host eq "rendezvous.example.com"` to the expression and use longer periods (for example 20 requests per minute, a 10-minute block).

#### Nginx

```nginx
# In the http block: 20 new connections per minute per IP
limit_req_zone $binary_remote_addr zone=puresend:10m rate=20r/m;

server {
    server_name p2p.example.com;

    location / {
        limit_req zone=puresend burst=5 nodelay;
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_read_timeout 86400s;
        proxy_send_timeout 86400s;
    }

    listen 443 ssl;
}
```

The standard Caddy build has no rate limiting; if you need it, use a Caddy built with the [caddy-ratelimit](https://github.com/mholt/caddy-ratelimit) plugin, or the Nginx configuration above.

Sudden jumps in the `puresend_lookups_throttled_total` and `libp2p_rcmgr_blocked_resources` metrics show that the edge limit is not enough.

---

## 4. Verifying the Deployment and Health Check

### 1. Local Health Check
Test the JSON health output on the server:
```bash
curl http://localhost:8081/health
```
**Expected response** (`version` is the build's `FT_VERSION`; `dev` if you built without one):
```json
{"status":"ok","version":"2.0.7","peer_id":"12D3KooW...","active_rooms":0}
```

### 2. WebSocket Handshake Test From Outside
Test the WebSocket connection from a network outside the server:
```bash
curl -sI https://p2p.example.com \
     -H "Connection: Upgrade" -H "Upgrade: websocket"
```
**Expected response:** `HTTP/1.1 101 Switching Protocols`.

---

## 5. Security Hardening and Monitoring

### 5.1 Docker Security
The provided `docker-compose.yml` comes with these hardening measures:
* **Read-only filesystem (`read_only: true`):** No malicious file can be written to the container's root.
* **Privilege isolation (`cap_drop: ALL`, `no-new-privileges: true`):** Root privilege escalation is blocked.
* **Port isolation:** `8080` and `8081` listen only on `127.0.0.1`; they are never exposed directly to the outside world.

### 5.2 Prometheus Metrics
The server publishes metrics at `http://localhost:8081/metrics`. The notable ones:

| Metric | Meaning |
|---|---|
| `puresend_active_rooms` | Number of transfer rooms active right now |
| `puresend_rooms_expired_total` | Rooms closed after timing out |
| `puresend_rooms_evicted_total` | Rooms whose owner had left, dropped early to make space when the table is full |
| `puresend_lookups_throttled_total` | Room lookups that hit the rate limit |
| `libp2p_relaysvc_data_transferred_bytes_total` | Amount of data flowing through the relay (bytes) |
| `libp2p_rcmgr_blocked_resources` | Excessive requests rejected by the resource manager |

### 5.3 Backing Up the Server Identity Key (Recovery)
To keep the server key in your password manager:
```bash
docker compose exec rendezvous base64 -w0 /data/server.key
```
To use the same identity on a new or different server, just assign this output to the `FT_IDENTITY_KEY` variable in the compose environment:
```yaml
environment:
  - FT_IDENTITY_KEY=CAESQ...
```

---

## 6. Releasing and Signing

Clients are built and uploaded to GitHub Releases by `.github/workflows/release.yml` when a `v*` tag is pushed. Two rules apply:

### 6.1 A published release is never changed

The workflow refuses to publish for a tag that already has a release. The digests of published files have been checked by users, `puresend -update`, the install scripts and the AUR `PKGBUILD`; re-publishing under the same tag would invalidate all of those checks. A fix is a new version number (`v1.0.1`). If the repository's **Settings → General → Releases** has GitHub's *release immutability* setting, turn it on; the tag and the files are then locked on GitHub's side too.

### 6.2 `checksums.txt` is signed with minisign

`puresend -update` and the install scripts always compare the archive they download with `checksums.txt`. That catches a corrupted or tampered download, but whoever can change the release page can change the list too. The signature ties the list to a key kept somewhere else.

One-time setup (the key is generated without a passphrase, because the workflow cannot type one):

```bash
minisign -G -W -p puresend.pub -s puresend.key
```

1. **Settings → Secrets and variables → Actions → Secrets:** `MINISIGN_SECRET_KEY` = the whole content of `puresend.key`.
2. **Settings → Secrets and variables → Actions → Variables:** `FT_UPDATE_KEY` = the **second line** of `puresend.pub` (it starts with `RW...`).
3. Write the same `RW...` line into `PUBKEY=""` in `install.sh` and `$pubKey = ""` in `install.ps1`. Both scripts verify the signature only if `minisign` is installed on the machine; the SHA-256 check always runs.
4. Keep `puresend.key` in your password manager or somewhere offline and do not put it in the repository (`.gitignore` already excludes `*.key` files).

From then on every release also contains `checksums.txt.minisig`, and `FT_UPDATE_KEY` is embedded in the clients. The workflow builds nothing and stops if only one of the two values is set, or if the secret key does not match the public key; clients shipped with a mismatched key could never update themselves again.

> ⚠️ Clients with the key embedded refuse to update to a release that is unsigned or signed with another key. Losing the secret key means these clients cannot be updated with `-update`; users have to reinstall with the install script.

To verify a release by hand:

```bash
minisign -Vm checksums.txt -P RWQ2F1ZFuTGorH4GqU4qC3PzJo5Evx2OKfNfJSiLbgyoEkMFDwUV8Kts
sha256sum --ignore-missing -c checksums.txt
```

### 6.3 Server Address and `server.txt`

Every client gets the meeting point's address embedded together with its Peer ID: `FT_SERVER` if the repository variable is set, otherwise the default in `release.yml`. If the address is wrong, it is noticed only after people have downloaded the files. That is why the workflow checks that the address it is about to embed is listed in `FT_SERVER_LIST` (default `https://puresend.madebybaki.com/server.txt`), and builds nothing if it is not. If the two disagree, one of them is stale; most often it is the default that was not updated after the server key changed. If the list cannot be downloaded at that moment, the workflow only warns and carries on.

When the server key changes (see 5.3), the order is: first add the new address to `server.txt`; older releases look there when they cannot reach the embedded address. Then update the `FT_SERVER` variable or the default in the workflow, and tag the new release last.

### 6.4 Release Order

1. Move the `[Unreleased]` notes in `docs/CHANGELOG.md` under the new version's heading (`## [2.0.2] - YYYY-MM-DD`); the workflow rejects a tag that has no heading.
2. **Push the tag first, then `main`:** `git push origin v2.0.2`, and `git push origin main` once the workflow has finished and the release is live. The install scripts are downloaded straight from `main`; if a new public key or a new rule reaches `main` before the release that satisfies it is out, the script can reject the then-latest release.
3. Verify the release: is `checksums.txt.minisig` published, does the signature verify with the public key (6.2), do the archives match `checksums.txt`.
4. The downloads on the website are **the release's own files**: extracted from the verified archives, never built locally. A local build does not embed `FT_UPDATE_KEY` (that copy would accept an unsigned update too), and the file would not match the signed list.
5. Bring `pkgver` and the archive digest in `packaging/PKGBUILD` up to the new version; the digest is known only after the release is built.

---

## 7. Secrets: Inventory, Storage and Rotation

The project holds two secret keys. Every other value (the server address, `server.txt`, the public half of the signing key) is public and may stay that way.

| Key | Where it lives | If lost | If leaked |
| :--- | :--- | :--- | :--- |
| **Server identity key** (`server.key` / `FT_IDENTITY_KEY`) | On the server in the `rendezvous-key` volume (`/data/server.key`); backed up in the password manager (5.3) | The Peer ID changes. Released clients cannot reach the embedded address and find the new one only through `server.txt`. | Using the key also requires taking over the domain's traffic; someone who can do that can already redirect clients to their own server through `server.txt`. The server does not see the files or the secret words of the codes (`SECURITY.md`). Rotating it is expensive (7.3) and not urgent. |
| **Signing key** (minisign, `MINISIGN_SECRET_KEY`) | In the GitHub Actions secret and in the password manager | Clients with the key embedded cannot be updated with `-update`; users reinstall with the install script. | Someone who can change the release page can publish an update that looks signed. Rotate immediately (7.2). |

What does not need storing: the `GITHUB_TOKEN` in Actions is issued by GitHub on every run; clients generate a new identity on every start and store it nowhere. The Cloudflare Tunnel credentials file on the server (`/root/.cloudflared/<tunnel>.json`) is secret but can be regenerated from the Cloudflare dashboard.

### 7.1 Rules

- Never put a secret key in any chat, issue, PR, log or screenshot, including conversations with AI assistants; those tools store the conversation in plain text on disk and at the provider. When talking about a key, share its name or key ID.
- Generate keys outside the repository, in a folder only you can read (`umask 077`). `.gitignore` excludes `*.key` files, but that is a safety net, not a method.
- A minisign secret key file is two lines: `untrusted comment: ...` and the key itself. Put both in the password manager and in the GitHub secret; minisign always reads the first line as a comment and cannot sign with a one-line file.
- Once a GitHub secret is saved, nobody, the repository owner included, can read it back; the only readable copy is the one in the password manager. Save it to the password manager first, then to GitHub.
- Under **Settings → Secrets and variables → Actions** on GitHub, secrets go to the *Secrets* tab and the public key to the *Variables* tab, both at the **Repository** level (not under an Environment).

### 7.2 Rotating the Signing Key

1. Generate a new pair outside the repository:
   ```bash
   umask 077; mkdir -p ~/puresend-signing && cd ~/puresend-signing
   minisign -G -W -p puresend.pub -s puresend.key
   ```
2. Replace the old one in the password manager with the two lines of `puresend.key`.
3. On GitHub, update the `MINISIGN_SECRET_KEY` secret (the whole file) and the `FT_UPDATE_KEY` variable (the second line of `puresend.pub`).
4. Write the new public key into `install.sh` (`PUBKEY`), `install.ps1` (`$pubKey`), `SECURITY.md` and the verification example in 6.2; check with `grep` that the old public key remains nowhere else in the repository.
5. Publish a new release (6.4) and delete the `~/puresend-signing` folder.

> ⚠️ Clients with the old key embedded reject a release signed with the new key. If the key changes after a release that carries it is published, that release's users cannot update with `-update`; tell them in the release notes to reinstall with the install script. If the key leaked, this price still has to be paid.

### 7.3 Rotating the Server Key

1. Generate the new key outside the repository and learn its Peer ID; if the server finds no file at a key path it generates a new one and prints the Peer ID:
   ```bash
   umask 077; go run ./cmd/server -key ~/new-server.key -ws-port 18080 -health-addr ""
   # note the "Peer ID: 12D3KooW..." line, stop with Ctrl+C
   base64 -w0 ~/new-server.key   # this output goes to the password manager
   ```
2. **Add** the new address (`/dns4/<domain>/tcp/443/tls/ws/p2p/<new Peer ID>`) to `server.txt`; do not delete the old one yet.
3. Set `FT_IDENTITY_KEY` to the new value on the server (5.3) and restart it. Old clients that cannot reach the embedded address find the new address in `server.txt`.
4. Update the `FT_SERVER` variable or the default in `release.yml` and publish a new release (6.3, 6.4).
5. Remove the old address from `server.txt` and delete `~/new-server.key`.
