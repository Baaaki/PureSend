package tui

import "time"

// rateMeter turns a stream of "bytes done so far" reports into the two
// numbers people actually watch: how fast the transfer is going, and how
// long is left.
//
// It smooths rather than reporting either extreme. The instantaneous rate
// between two progress events jumps around far too much to read, and the
// average over the whole transfer takes minutes to notice that the link
// just got worse. An exponential moving average over samples a few hundred
// milliseconds apart sits between the two: quick enough to react, steady
// enough to look at.
type rateMeter struct {
	sampledAt time.Time
	sampled   int64
	smoothed  float64 // bytes per second

	// warmup is the very first sample, kept so a short transfer that never
	// reaches a second sample still reports something honest.
	startedAt  time.Time
	startedAt0 int64
	latest     int64
}

const (
	// sampleInterval is how often the rate is recomputed. Shorter makes
	// the number twitchy; longer makes it feel stale.
	sampleInterval = 400 * time.Millisecond

	// smoothing is the weight given to the newest sample.
	smoothing = 0.35
)

// reset starts a fresh measurement. done is where the transfer begins,
// which is not always zero: a resumed transfer starts partway through, and
// counting the bytes it never transferred would report a wild speed.
func (r *rateMeter) reset(done int64) {
	now := time.Now()
	*r = rateMeter{
		sampledAt:  now,
		sampled:    done,
		startedAt:  now,
		startedAt0: done,
		latest:     done,
	}
}

// observe records the latest byte count.
func (r *rateMeter) observe(done int64) {
	if r.sampledAt.IsZero() {
		r.reset(done)
		return
	}
	r.latest = done
	elapsed := time.Since(r.sampledAt)
	if elapsed < sampleInterval {
		return
	}

	instant := float64(done-r.sampled) / elapsed.Seconds()
	if instant < 0 {
		instant = 0 // a new file restarting the count, not a negative speed
	}
	if r.smoothed == 0 {
		r.smoothed = instant
	} else {
		r.smoothed = smoothing*instant + (1-smoothing)*r.smoothed
	}
	r.sampledAt, r.sampled = time.Now(), done
}

// rate is the current speed in bytes per second, or 0 while it is still
// too early to say anything meaningful.
func (r *rateMeter) rate() float64 {
	if r.smoothed > 0 {
		return r.smoothed
	}
	// No full sample yet. A transfer that finishes inside one interval
	// still deserves a number, so fall back to the plain average.
	if r.startedAt.IsZero() {
		return 0
	}
	elapsed := time.Since(r.startedAt).Seconds()
	if elapsed < 0.2 || r.latest <= r.startedAt0 {
		return 0
	}
	return float64(r.latest-r.startedAt0) / elapsed
}

// eta estimates how long the remaining bytes will take. The second return
// value is false when there is nothing sensible to say — no measured
// speed, or nothing left to transfer.
func (r *rateMeter) eta(remaining int64) (time.Duration, bool) {
	rate := r.rate()
	if rate <= 0 || remaining <= 0 {
		return 0, false
	}
	return time.Duration(float64(remaining) / rate * float64(time.Second)), true
}
