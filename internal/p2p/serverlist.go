package p2p

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"puresend/internal/minisign"
)

// The meeting point's address is baked into every released copy of the
// program, peer ID and all. If that ever changes — the identity key is
// lost, the server moves to a new host — every copy already downloaded
// stops working at once, and a new release only helps the people who
// fetch it.
//
// The server list is the escape hatch: a small text file at a fixed HTTPS
// address (the landing page serves it), listing the current addresses.
// A client consults it only when none of its built-in addresses answers,
// so it costs nothing in the normal case and keeps old copies alive when
// the server has moved.
//
// The list is not trusted any more than the server is. Whoever controls it
// can point clients at a meeting point of their own, which learns who
// meets whom — but cannot read or alter a transfer: a meeting point is
// only ever told a code's nameplate, and both ends prove the whole code,
// secret words included, to each other. When serverListPublicKey is set,
// the list must also carry a valid minisign signature (.minisig) so even a
// compromised CDN cannot redirect clients to a metadata-harvesting server.
const (
	serverListTimeout   = 10 * time.Second
	maxServerListBytes  = 16 << 10
	maxServerListSigLen = 4 << 10
	maxListedServers    = 16
)

// allowInsecureServerList permits plain HTTP URLs only in unit tests using
// httptest.NewServer. Production builds always require HTTPS.
var allowInsecureServerList = false

// serverListPublicKey is an optional minisign public key that server.txt must
// be signed with (at <url>.minisig), configurable via -ldflags:
//
//	-ldflags "-X puresend/internal/p2p.serverListPublicKey=RWT..."
var serverListPublicKey = ""

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateServerListURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if strings.EqualFold(u.Scheme, "https") {
		return nil
	}
	if strings.EqualFold(u.Scheme, "http") && (allowInsecureServerList || isLoopbackHost(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("refusing insecure server list scheme %q", u.Scheme)
}

func fetchBoundedURL(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	if err := validateServerListURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: serverListTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return validateServerListURL(req.URL.String())
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("exceeds maximum size (%d bytes)", limit)
	}
	return data, nil
}

// fetchServerList downloads the list: one multiaddr per line, blank lines
// and lines starting with # ignored.
func fetchServerList(ctx context.Context, rawURL string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, serverListTimeout)
	defer cancel()

	body, err := fetchBoundedURL(ctx, rawURL, maxServerListBytes)
	if err != nil {
		return nil, fmt.Errorf("server list: %w", err)
	}

	if serverListPublicKey != "" {
		key, err := minisign.ParseKey(serverListPublicKey)
		if err != nil {
			return nil, fmt.Errorf("server list: built-in public key is invalid: %w", err)
		}
		sig, err := fetchBoundedURL(ctx, strings.TrimSpace(rawURL)+".minisig", maxServerListSigLen)
		if err != nil {
			return nil, fmt.Errorf("server list signature: %w", err)
		}
		if err := minisign.Verify(key, body, sig); err != nil {
			return nil, fmt.Errorf("server list signature: %w", err)
		}
	}

	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() && len(out) < maxListedServers {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("server list: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("server list: no addresses")
	}
	return out, nil
}
