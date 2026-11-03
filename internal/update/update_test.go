package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"puresend/internal/i18n"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		latest  string
		current string
		want    bool
	}{
		{"0.3.0", "0.2.0", true},
		{"0.2.1", "0.2.0", true},
		{"1.0.0", "0.9.9", true},
		{"v0.3.0", "v0.2.0", true},
		{"v0.3.0-rc1", "v0.2.0", true},
		{"0.2.0", "0.2.0", false},
		{"v0.2.0", "0.2.0", false},
		{"0.1.9", "0.2.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"0.2.0", "dev", true},
		{"v1.0.0", "unknown", true},
		{"v1.0.0", "", true},
		{"invalid", "0.2.0", false},
	}

	for _, tt := range tests {
		got := IsNewer(tt.latest, tt.current)
		if got != tt.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}

func TestFindAsset(t *testing.T) {
	assets := []Asset{
		{Name: "puresend-checksums.txt.sha256"},
		{Name: "puresend-linux-amd64", BrowserDownloadURL: "https://example.com/linux-amd64"},
		{Name: "puresend-windows-amd64.exe", BrowserDownloadURL: "https://example.com/win-amd64"},
		{Name: "puresend-darwin-arm64", BrowserDownloadURL: "https://example.com/mac-arm64"},
	}

	foundLinux := FindAsset(assets, "linux", "amd64")
	if foundLinux == nil || foundLinux.BrowserDownloadURL != "https://example.com/linux-amd64" {
		t.Errorf("FindAsset(linux, amd64) failed: %v", foundLinux)
	}

	foundMac := FindAsset(assets, "darwin", "arm64")
	if foundMac == nil || foundMac.BrowserDownloadURL != "https://example.com/mac-arm64" {
		t.Errorf("FindAsset(darwin, arm64) failed: %v", foundMac)
	}

	foundWin := FindAsset(assets, "windows", "amd64")
	if foundWin == nil || foundWin.BrowserDownloadURL != "https://example.com/win-amd64" {
		t.Errorf("FindAsset(windows, amd64) failed: %v", foundWin)
	}

	assetsGoReleaser := []Asset{
		{Name: "checksums.txt"},
		{Name: "puresend_0.3.0_linux_x86_64.tar.gz", BrowserDownloadURL: "https://example.com/linux-x86_64.tar.gz"},
		{Name: "puresend_0.3.0_windows_x86_64.zip", BrowserDownloadURL: "https://example.com/win-x86_64.zip"},
		{Name: "puresend_0.3.0_macOS_arm64.tar.gz", BrowserDownloadURL: "https://example.com/mac-arm64.tar.gz"},
		{Name: "puresend_0.3.0_amd64.deb"},
	}

	foundLinuxGR := FindAsset(assetsGoReleaser, "linux", "amd64")
	if foundLinuxGR == nil || foundLinuxGR.BrowserDownloadURL != "https://example.com/linux-x86_64.tar.gz" {
		t.Errorf("FindAsset(linux, amd64) for GoReleaser naming failed: %v", foundLinuxGR)
	}

	foundMacGR := FindAsset(assetsGoReleaser, "darwin", "arm64")
	if foundMacGR == nil || foundMacGR.BrowserDownloadURL != "https://example.com/mac-arm64.tar.gz" {
		t.Errorf("FindAsset(darwin, arm64) for GoReleaser naming failed: %v", foundMacGR)
	}

	foundWinGR := FindAsset(assetsGoReleaser, "windows", "amd64")
	if foundWinGR == nil || foundWinGR.BrowserDownloadURL != "https://example.com/win-x86_64.zip" {
		t.Errorf("FindAsset(windows, amd64) for GoReleaser naming failed: %v", foundWinGR)
	}
}

func TestCheckLatest(t *testing.T) {
	mockRelease := ReleaseInfo{
		TagName: "v0.3.0",
		Name:    "Version 0.3.0",
		HTMLURL: "https://github.com/Baaaki/PureSend/releases/tag/v0.3.0",
		Assets: []Asset{
			{Name: "puresend-linux-amd64", BrowserDownloadURL: "https://example.com/bin"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockRelease)
	}))
	defer srv.Close()

	// Override endpoint for testing
	origEndpoint := endpointURL
	endpointURL = srv.URL
	defer func() { endpointURL = origEndpoint }()

	// Test with older version
	info, err := CheckLatest(context.Background(), "v0.2.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if !info.HasUpdate {
		t.Errorf("Expected HasUpdate=true for v0.2.0")
	}
	if info.TagName != "v0.3.0" {
		t.Errorf("Expected TagName=v0.3.0, got: %s", info.TagName)
	}

	// Test with current version
	info2, err := CheckLatest(context.Background(), "v0.3.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if info2.HasUpdate {
		t.Errorf("Expected HasUpdate=false for v0.3.0")
	}
}

func TestCheckLatest404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	origEndpoint := endpointURL
	endpointURL = srv.URL
	defer func() { endpointURL = origEndpoint }()

	info, err := CheckLatest(context.Background(), "dev")
	if err != nil {
		t.Fatalf("CheckLatest 404 should not error: %v", err)
	}
	if info.HasUpdate {
		t.Errorf("Expected HasUpdate=false on 404")
	}
}

func TestApplyUpToDate(t *testing.T) {
	mockRelease := ReleaseInfo{
		TagName: "v0.2.0",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockRelease)
	}))
	defer srv.Close()

	origEndpoint := endpointURL
	endpointURL = srv.URL
	defer func() { endpointURL = origEndpoint }()

	// It answers in the language it is asked in.
	for lang, want := range map[i18n.Lang]string{i18n.TR: "zaten güncel", i18n.EN: "is up to date"} {
		var buf bytes.Buffer
		if err := Apply("v0.2.0", lang, &buf); err != nil {
			t.Fatalf("Apply(%s): %v", lang, err)
		}
		if !strings.Contains(buf.String(), want) {
			t.Errorf("Apply(%s) said %q, want it to contain %q", lang, buf.String(), want)
		}
	}
}

// TestManagedByPackage: the .deb and the AUR package own /usr/bin. What
// install.sh installs, into /usr/local/bin or ~/.local/bin, updates itself.
func TestManagedByPackage(t *testing.T) {
	for _, tc := range []struct {
		goos, exe string
		want      bool
	}{
		{"linux", "/usr/bin/puresend", true},
		{"linux", "/bin/puresend", true},
		{"linux", "/usr/local/bin/puresend", false},
		{"linux", "/home/ali/.local/bin/puresend", false},
		{"linux", "/opt/puresend/puresend", false},
		{"darwin", "/usr/bin/puresend", false},
		{"windows", `C:\Users\ali\AppData\Local\PureSend\puresend.exe`, false},
	} {
		if got := managedByPackage(tc.goos, tc.exe); got != tc.want {
			t.Errorf("managedByPackage(%s, %s) = %v, want %v", tc.goos, tc.exe, got, tc.want)
		}
	}
}

func TestExtractBinary(t *testing.T) {
	content := []byte("fake-binary-content-12345")

	// 1. Test plain binary
	var outPlain bytes.Buffer
	if err := extractBinary("puresend", bytes.NewReader(content), &outPlain); err != nil {
		t.Fatalf("extractBinary plain failed: %v", err)
	}
	if !bytes.Equal(outPlain.Bytes(), content) {
		t.Errorf("expected %s, got %s", content, outPlain.Bytes())
	}

	// 2. Test tar.gz
	var tgzBuf bytes.Buffer
	gw := gzip.NewWriter(&tgzBuf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name: "puresend",
		Mode: 0o755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gw.Close()

	var outTar bytes.Buffer
	if err := extractBinary("puresend_0.3.0_linux_x86_64.tar.gz", &tgzBuf, &outTar); err != nil {
		t.Fatalf("extractBinary tar.gz failed: %v", err)
	}
	if !bytes.Equal(outTar.Bytes(), content) {
		t.Errorf("expected %s, got %s", content, outTar.Bytes())
	}

	// 3. Test zip
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	zf, err := zw.Create("puresend.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zf.Write(content); err != nil {
		t.Fatal(err)
	}
	_ = zw.Close()

	var outZip bytes.Buffer
	if err := extractBinary("puresend_0.3.0_windows_x86_64.zip", &zipBuf, &outZip); err != nil {
		t.Fatalf("extractBinary zip failed: %v", err)
	}
	if !bytes.Equal(outZip.Bytes(), content) {
		t.Errorf("expected %s, got %s", content, outZip.Bytes())
	}
}

func TestFindAssetExcludesServerSignaturesAndPackages(t *testing.T) {
	assets := []Asset{
		{Name: "puresend-server_0.3.0_linux_amd64.tar.gz"},
		{Name: "puresend_0.3.0_linux_amd64.tar.gz.minisig"},
		{Name: "puresend_0.3.0_linux_amd64.tar.gz.sig"},
		{Name: "puresend_0.3.0_linux_amd64.tar.gz.asc"},
		{Name: "puresend_0.3.0_linux_amd64.rpm"},
		{Name: "puresend_0.3.0_linux_amd64.apk"},
		{Name: "puresend_0.3.0_linux_amd64.sbom.json"},
		{Name: "puresend_0.3.0_linux_amd64.txt"},
		{Name: "puresend_0.3.0_darwin_arm64.dmg"},
		{Name: "puresend_0.3.0_darwin_arm64.pkg"},
		{Name: "puresend_0.3.0_windows_amd64.msi"},
		{Name: "puresend_0.3.0_linux_amd64.tar.gz"},
	}

	got := FindAsset(assets, "linux", "amd64")
	if got == nil || got.Name != "puresend_0.3.0_linux_amd64.tar.gz" {
		t.Fatalf("FindAsset(linux/amd64) = %+v, want puresend_0.3.0_linux_amd64.tar.gz", got)
	}
	if got := FindAsset(assets[:len(assets)-1], "linux", "amd64"); got != nil {
		t.Errorf("FindAsset matched excluded asset: %+v", got)
	}
	if got := FindAsset(assets, "darwin", "arm64"); got != nil {
		t.Errorf("FindAsset matched installer package on darwin: %+v", got)
	}
	if got := FindAsset(assets, "windows", "amd64"); got != nil {
		t.Errorf("FindAsset matched msi installer on windows: %+v", got)
	}
}

func TestCopyBoundedRejectsOversizedStream(t *testing.T) {
	var dst bytes.Buffer
	src := bytes.NewReader(bytes.Repeat([]byte("a"), 1025))
	if err := copyBounded(&dst, src, 1024); err == nil {
		t.Fatal("expected copyBounded to fail when stream exceeds limit")
	}
}
