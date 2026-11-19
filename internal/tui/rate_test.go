package tui

import (
	"testing"
	"time"
)

// TestRateMeterMeasuresSpeed feeds a steady 1 MB/s and checks the meter
// lands near it.
func TestRateMeterMeasuresSpeed(t *testing.T) {
	var r rateMeter
	r.reset(0)

	// Backdate the samples instead of sleeping: this has to run in a test
	// suite, not in real time.
	const mb = 1 << 20
	for i := 1; i <= 6; i++ {
		r.sampledAt = time.Now().Add(-500 * time.Millisecond)
		r.observe(int64(i) * mb / 2) // half a megabyte every half second
	}

	got := r.rate()
	if got < 0.8*mb || got > 1.2*mb {
		t.Errorf("rate = %.0f B/s, want roughly %d B/s", got, mb)
	}

	eta, ok := r.eta(10 * mb)
	if !ok {
		t.Fatal("no estimate from a measured rate")
	}
	if eta < 8*time.Second || eta > 12*time.Second {
		t.Errorf("eta = %v, want about 10s", eta)
	}
}

// TestRateMeterIgnoresResumedBytes is the trap resume sets: a transfer
// that starts 900 MB in has not moved 900 MB in its first instant, and
// reporting that would show an absurd speed and an estimate of zero.
func TestRateMeterIgnoresResumedBytes(t *testing.T) {
	var r rateMeter
	const mb = 1 << 20
	r.reset(900 * mb)

	r.sampledAt = time.Now().Add(-time.Second)
	r.observe(901 * mb)

	if got := r.rate(); got > 2*mb {
		t.Errorf("rate = %.0f B/s, want about %d B/s — the resumed bytes were counted", got, mb)
	}
}

// TestRateMeterSaysNothingTooEarly checks that the interface is not handed
// a made-up number before there is anything to measure.
func TestRateMeterSaysNothingTooEarly(t *testing.T) {
	var r rateMeter
	if got := r.rate(); got != 0 {
		t.Errorf("rate = %v before any sample, want 0", got)
	}
	if _, ok := r.eta(1 << 20); ok {
		t.Error("an estimate was offered before any speed was measured")
	}

	r.reset(0)
	r.observe(1 << 20) // too soon to close a sample
	if _, ok := r.eta(0); ok {
		t.Error("an estimate was offered with nothing left to transfer")
	}
}

// TestRateMeterWarmupBeforeFirstSample ensures short transfers between 200ms
// and sampleInterval (400ms) report a non-zero average rate instead of 0.
func TestRateMeterWarmupBeforeFirstSample(t *testing.T) {
	var r rateMeter
	const mb = 1 << 20
	r.reset(0)
	r.startedAt = time.Now().Add(-300 * time.Millisecond)
	r.sampledAt = r.startedAt
	r.observe(3 * mb)

	got := r.rate()
	if got <= 0 {
		t.Fatalf("warmup rate = %v, want positive rate", got)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		3 * time.Second:                 "birkaç saniye",
		45 * time.Second:                "~45 saniye",
		90 * time.Second:                "~2 dakika",
		2*time.Hour + 30*time.Minute:    "~2 saat 30 dakika",
		time.Duration(0):                "birkaç saniye",
		10*time.Minute + 30*time.Second: "~11 dakika",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFormatRate(t *testing.T) {
	for bps, want := range map[float64]string{
		512:           "512 B/sn",
		2048:          "2 KB/sn",
		5 * (1 << 20): "5.0 MB/sn",
	} {
		if got := formatRate(bps); got != want {
			t.Errorf("formatRate(%v) = %q, want %q", bps, got, want)
		}
	}
}
