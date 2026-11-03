package headless

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"puresend/internal/i18n"
	"puresend/internal/p2p"
	"puresend/internal/transfer"
)

// TestDescribeConnection: the sender's side never learns the relay's
// limit, and a relayed sender used to be told its relay carried "0 B".
func TestDescribeConnection(t *testing.T) {
	for _, tc := range []struct {
		event p2p.ConnectedEvent
		want  string
	}{
		{p2p.ConnectedEvent{Direct: true, RelayLimit: 256 << 20}, "connected directly"},
		{p2p.ConnectedEvent{RelayLimit: 256 << 20}, "connected through the fallback relay (limit 256.0 MB per connection)"},
		{p2p.ConnectedEvent{}, "connected through the fallback relay"},
	} {
		got := describeConnection(tc.event)
		if got != tc.want {
			t.Errorf("describeConnection(%+v) = %q, want %q", tc.event, got, tc.want)
		}
		if strings.Contains(got, "0 B") {
			t.Errorf("describeConnection(%+v) = %q mentions a limit of nothing", tc.event, got)
		}
	}
}

func TestAskYesNoCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if askYesNo(ctx, i18n.EN) {
		t.Error("askYesNo should return false when ctx is cancelled")
	}
}

func TestFormatManifestTruncationAndHiddenWarning(t *testing.T) {
	var files []transfer.FileInfo
	for i := range 25 {
		files = append(files, transfer.FileInfo{
			Path: fmt.Sprintf("folder/file-%02d.txt", i),
			Size: 100,
		})
	}
	files = append(files, transfer.FileInfo{
		Path: ".config/autostart/evil.desktop",
		Size: 100,
	})

	lines := formatManifest(transfer.Manifest{Files: files}, i18n.EN)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "and 6 more files") {
		t.Errorf("expected truncation summary for >20 files, got:\n%s", joined)
	}
	if !strings.Contains(joined, "warning: includes hidden files/folders (.config)") {
		t.Errorf("expected hidden file warning, got:\n%s", joined)
	}
}
