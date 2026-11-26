package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"puresend/internal/i18n"
	"puresend/internal/minisign"
)

const (
	defaultTimeout = 5 * time.Second
	defaultRepo    = "Baaaki/PureSend"
)

// Endpoint URL can be overridden in tests.
var endpointURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", defaultRepo)

// allowInsecureDownload permits plain HTTP asset downloads only in unit tests
// using httptest.NewServer. Production builds always require HTTPS.
var allowInsecureDownload = false

// publicKey is the minisign key a release's checksums.txt must be signed
// with. Defaults to the official project signing key so source builds also
// verify signatures; can be overridden or cleared at build time:
//
//	-ldflags "-X puresend/internal/update.publicKey=RWT..."
//
// Empty skips the signature check while still requiring checksums.txt.
var publicKey = minisign.DefaultPublicKey

// Bounds on what an update downloads, so a hostile or broken server cannot
// make it hold an endless response in memory.
const (
	maxChecksumBytes  = 64 << 10
	maxSignatureBytes = 4 << 10
	maxArchiveBytes   = 256 << 20
)

// ReleaseInfo holds the details of a GitHub release.
type ReleaseInfo struct {
	TagName      string  `json:"tag_name"`
	Name         string  `json:"name"`
	HTMLURL      string  `json:"html_url"`
	Body         string  `json:"body"`
	Assets       []Asset `json:"assets"`
	HasUpdate    bool    `json:"-"`
	CleanVersion string  `json:"-"`
}

// Asset represents a downloadable binary or package in a GitHub release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// parseVersion extracts integer components and optional pre-release suffix
// from a semver string (e.g. "v0.3.1-rc.1" -> [0, 3, 1], "rc.1").
func parseVersion(v string) ([]int, string, bool) {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	v = strings.TrimSpace(v)
	if idx := strings.IndexByte(v, '+'); idx != -1 {
		v = v[:idx]
	}
	var prerelease string
	if idx := strings.IndexByte(v, '-'); idx != -1 {
		prerelease = v[idx+1:]
		v = v[:idx]
	}
	if v == "" {
		return nil, "", false
	}
	parts := strings.Split(v, ".")
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, "", false
		}
		nums[i] = n
	}
	return nums, prerelease, true
}

// IsNewer reports whether the latest version is strictly newer than current.
func IsNewer(latest, current string) bool {
	latestNums, latestPre, okLatest := parseVersion(latest)
	if !okLatest {
		return false
	}
	currentNums, currentPre, okCurrent := parseVersion(current)
	if !okCurrent {
		// Non-semver current (e.g. "dev", "unknown") is considered older than a valid release.
		return true
	}
	maxLen := max(len(latestNums), len(currentNums))
	for i := 0; i < maxLen; i++ {
		l := 0
		if i < len(latestNums) {
			l = latestNums[i]
		}
		c := 0
		if i < len(currentNums) {
			c = currentNums[i]
		}
		if l > c {
			return true
		}
		if l < c {
			return false
		}
	}
	// Equal numeric versions: a final release is newer than a pre-release.
	if latestPre == "" && currentPre != "" {
		return true
	}
	if latestPre != "" && currentPre == "" {
		return false
	}
	return latestPre > currentPre
}

// CheckLatest queries GitHub for the latest release and compares it with currentVersion.
func CheckLatest(ctx context.Context, currentVersion string) (*ReleaseInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	ua := "puresend"
	if currentVersion != "" {
		ua += "/" + currentVersion
	}
	req.Header.Set("User-Agent", ua)

	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &ReleaseInfo{HasUpdate: false}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api returned status %d", resp.StatusCode)
	}

	var info ReleaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to parse release: %w", err)
	}

	info.CleanVersion = strings.TrimPrefix(info.TagName, "v")
	info.HasUpdate = IsNewer(info.TagName, currentVersion)
	return &info, nil
}

// FindAsset finds the most appropriate binary asset for the given GOOS and GOARCH.
func FindAsset(assets []Asset, targetOS, targetArch string) *Asset {
	osLower := strings.ToLower(targetOS)
	archLower := strings.ToLower(targetArch)

	osAliases := []string{osLower}
	switch osLower {
	case "darwin":
		osAliases = append(osAliases, "macos", "osx", "mac")
	case "windows":
		osAliases = append(osAliases, "win")
	}

	archAliases := []string{archLower}
	switch archLower {
	case "amd64":
		archAliases = append(archAliases, "x86_64", "x64", "64bit")
	case "arm64":
		archAliases = append(archAliases, "aarch64")
	}

	matchOS := func(name string) bool {
		// "darwin" ends in "win": without this, the Windows alias would
		// match a macOS archive named the Go way.
		name = strings.ReplaceAll(name, "darwin", "macos")
		for _, alias := range osAliases {
			if containsToken(name, alias) {
				return true
			}
		}
		return false
	}

	matchArch := func(name string) bool {
		for _, alias := range archAliases {
			if containsToken(name, alias) {
				return true
			}
		}
		return false
	}

	for i := range assets {
		name := strings.ToLower(assets[i].Name)
		if isExcludedAsset(name) {
			continue
		}
		if matchOS(name) && matchArch(name) {
			return &assets[i]
		}
	}
	return nil
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// containsToken reports whether needle appears in haystack bounded by
// non-alphanumeric characters (or string edges), so "arm" does not match "arm64".
func containsToken(haystack, needle string) bool {
	for start := 0; start <= len(haystack)-len(needle); {
		idx := strings.Index(haystack[start:], needle)
		if idx < 0 {
			return false
		}
		pos := start + idx
		end := pos + len(needle)
		leftOK := pos == 0 || !isAlphaNum(haystack[pos-1])
		rightOK := end == len(haystack) || !isAlphaNum(haystack[end])
		if leftOK && rightOK {
			return true
		}
		start = pos + 1
	}
	return false
}

func isExcludedAsset(name string) bool {
	if strings.Contains(name, "source") || strings.Contains(name, "server") || strings.Contains(name, ".sbom") {
		return true
	}
	for _, ext := range []string{
		".sha256", ".md5", ".minisig", ".sig", ".asc",
		".deb", ".rpm", ".apk", ".dmg", ".pkg", ".msi", ".txt", ".sbom",
	} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

func copyBounded(dst io.Writer, src io.Reader, limit int64) error {
	n, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("extracted binary exceeds maximum size (%d bytes)", limit)
	}
	return nil
}

// extractBinary writes the target binary to dst. If assetName is a tar.gz or zip, it extracts the binary entry.
func extractBinary(assetName string, r io.Reader, dst io.Writer) error {
	lower := strings.ToLower(assetName)
	if strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") {
		gr, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("could not read the gzip archive: %w", err)
		}
		defer func() { _ = gr.Close() }()

		tr := tar.NewReader(gr)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("could not read the tar archive: %w", err)
			}
			if hdr.Typeflag == tar.TypeReg {
				base := filepath.Base(hdr.Name)
				if base == "puresend" || base == "puresend.exe" {
					return copyBounded(dst, tr, maxArchiveBytes)
				}
			}
		}
		return errors.New("the archive holds no puresend executable")
	}

	if strings.HasSuffix(lower, ".zip") {
		buf, err := io.ReadAll(io.LimitReader(r, maxArchiveBytes+1))
		if err != nil {
			return fmt.Errorf("could not read the zip archive: %w", err)
		}
		if int64(len(buf)) > maxArchiveBytes {
			return fmt.Errorf("zip archive exceeds maximum size (%d bytes)", maxArchiveBytes)
		}
		zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
		if err != nil {
			return fmt.Errorf("could not read the zip archive: %w", err)
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || !f.FileInfo().Mode().IsRegular() {
				continue
			}
			base := filepath.Base(f.Name)
			if base == "puresend" || base == "puresend.exe" {
				rc, err := f.Open()
				if err != nil {
					return err
				}
				defer rc.Close()
				return copyBounded(dst, rc, maxArchiveBytes)
			}
		}
		return errors.New("the zip archive holds no puresend executable")
	}

	// Plain binary
	return copyBounded(dst, r, maxArchiveBytes)
}

// Apply checks for updates and replaces the running executable if a newer
// release exists. It talks to the person running it in lang; its errors,
// like every other error in the program, are in English.
func Apply(currentVersion string, lang i18n.Lang, stdout io.Writer) error {
	msg := i18n.Get(lang)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	_, _ = fmt.Fprintln(stdout, msg.UpdateChecking)
	info, err := CheckLatest(ctx, currentVersion)
	if err != nil {
		return fmt.Errorf("could not check for the latest version: %w", err)
	}

	if !info.HasUpdate {
		_, _ = fmt.Fprintln(stdout, msg.UpdateUpToDate(currentVersion))
		return nil
	}
	_, _ = fmt.Fprintln(stdout, msg.UpdateFound(info.TagName, currentVersion))

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not find the running program: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("could not resolve the program's path: %w", err)
	}
	if managedByPackage(runtime.GOOS, exePath) {
		_, _ = fmt.Fprintln(stdout, msg.UpdateManaged(exePath))
		_, _ = fmt.Fprintln(stdout, msg.UpdateGetItAt(info.HTMLURL))
		return nil
	}

	asset := FindAsset(info.Assets, runtime.GOOS, runtime.GOARCH)
	if asset == nil {
		_, _ = fmt.Fprintln(stdout, msg.UpdateNoBinary(runtime.GOOS+"/"+runtime.GOARCH))
		_, _ = fmt.Fprintln(stdout, msg.UpdateGetItAt(info.HTMLURL))
		return nil
	}

	// Download and check everything before touching the installed binary.
	_, _ = fmt.Fprintln(stdout, msg.UpdateDownloading(asset.Name))
	archive, err := fetchVerified(ctx, info.Assets, *asset, "puresend/"+currentVersion)
	if err != nil {
		return fmt.Errorf("the update could not be verified, nothing was changed: %w", err)
	}
	_, _ = fmt.Fprintln(stdout, msg.UpdateVerified)

	dir := filepath.Dir(exePath)
	tmpFile, err := os.CreateTemp(dir, "ft-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("no permission to write to %s (try again as an administrator, or with sudo): %w", dir, err)
		}
		return fmt.Errorf("could not create a temporary file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if err := extractBinary(asset.Name, bytes.NewReader(archive), tmpFile); err != nil {
		tmpFile.Close()
		return fmt.Errorf("could not write the new version: %w", err)
	}
	if err := tmpFile.Chmod(0o755); err != nil {
		tmpFile.Close()
		return fmt.Errorf("could not make the new version executable: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		// A running .exe cannot be replaced, but it can be renamed out of
		// the way.
		oldPath := exePath + ".old"
		_ = os.Remove(oldPath)
		if err := os.Rename(exePath, oldPath); err != nil {
			return fmt.Errorf("could not move the old version aside: %w", err)
		}
		if err := os.Rename(tmpPath, exePath); err != nil {
			// Put the old one back: failing to update must not leave the
			// user with no program at all.
			if rerr := os.Rename(oldPath, exePath); rerr != nil {
				return fmt.Errorf("could not install the new version (%w), nor put the old one back, which is at %s: %w", err, oldPath, rerr)
			}
			return fmt.Errorf("could not install the new version, the old one was left in place: %w", err)
		}
	} else if err := os.Rename(tmpPath, exePath); err != nil {
		return fmt.Errorf("could not install the new version: %w", err)
	}

	_, _ = fmt.Fprintln(stdout, msg.UpdateDone(info.TagName))
	return nil
}

// managedByPackage reports whether exe belongs to the system's package
// manager. The .deb and the AUR package both install to /usr/bin, where
// install.sh never puts anything. Replacing the file there behind the
// package manager's back leaves its records describing a file that is no
// longer the one it installed.
func managedByPackage(goos, exe string) bool {
	if goos != "linux" {
		return false
	}
	switch path.Dir(filepath.ToSlash(exe)) {
	case "/usr/bin", "/usr/sbin", "/bin", "/sbin":
		return true
	}
	return false
}

// fetchVerified downloads an asset and returns it only once it matches the
// digest the release's checksums.txt lists for it — and, in a build that
// carries a public key, only once checksums.txt is signed with that key.
// A release without a checksum file, or without a signature where one is
// expected, is refused rather than installed unchecked.
func fetchVerified(ctx context.Context, assets []Asset, asset Asset, userAgent string) ([]byte, error) {
	sumsAsset := assetNamed(assets, "checksums.txt")
	if sumsAsset == nil {
		return nil, errors.New("the release publishes no checksums.txt")
	}
	sums, err := download(ctx, sumsAsset.BrowserDownloadURL, userAgent, maxChecksumBytes)
	if err != nil {
		return nil, fmt.Errorf("could not download checksums.txt: %w", err)
	}

	if publicKey != "" {
		key, err := parseMinisignKey(publicKey)
		if err != nil {
			return nil, fmt.Errorf("the update key built into this program is not valid: %w", err)
		}
		sigAsset := assetNamed(assets, "checksums.txt.minisig")
		if sigAsset == nil {
			return nil, errors.New("the release's checksums.txt is not signed")
		}
		sig, err := download(ctx, sigAsset.BrowserDownloadURL, userAgent, maxSignatureBytes)
		if err != nil {
			return nil, fmt.Errorf("could not download the signature: %w", err)
		}
		if err := verifyMinisign(key, sums, sig); err != nil {
			return nil, fmt.Errorf("checksums.txt signature: %w", err)
		}
	}

	want, err := checksumFor(sums, asset.Name)
	if err != nil {
		return nil, err
	}
	data, err := download(ctx, asset.BrowserDownloadURL, userAgent, maxArchiveBytes)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s does not match its digest in checksums.txt", asset.Name)
	}
	return data, nil
}

// assetNamed finds a release asset by its exact name.
func assetNamed(assets []Asset, name string) *Asset {
	for i := range assets {
		if assets[i].Name == name {
			return &assets[i]
		}
	}
	return nil
}

// checksumFor finds a file's SHA-256 in a checksums.txt ("<hex>  <name>"
// per line, as sha256sum and GoReleaser write it).
func checksumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		digest := strings.ToLower(fields[0])
		if len(digest) != 2*sha256.Size {
			break
		}
		if _, err := hex.DecodeString(digest); err != nil {
			break
		}
		return digest, nil
	}
	return "", fmt.Errorf("checksums.txt has no valid digest for %s", name)
}

func validateDownloadURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid download URL: %w", err)
	}
	if !allowInsecureDownload && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("refusing insecure download URL scheme %q", u.Scheme)
	}
	return nil
}

// download fetches a URL into memory, refusing anything over limit bytes
// and rejecting non-HTTPS URLs or HTTPS-to-HTTP redirects in production.
func download(ctx context.Context, rawURL, userAgent string, limit int64) ([]byte, error) {
	if err := validateDownloadURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return validateDownloadURL(req.URL.String())
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("larger than expected (at most %d bytes)", limit)
	}
	return data, nil
}
