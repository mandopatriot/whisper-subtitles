package media

// Where the plugin listens: Windows windows of WindowMs, spread evenly over
// the runtime less the end credits. The credits are mostly music; a window
// there hears nothing to match, and one such window hung Whisper Pro ASR's
// language detection.
const (
	Windows      = 12
	WindowMs     = 90_000
	CreditsMinMs = 180_000 // the end left out: at least this,
	CreditsShare = 0.08    // or this share of the runtime, whichever is more
)

// WindowStarts gives the start of each window for a video of durationMs.
// Window i starts at (2i+1)/(2n) of the runtime less the credits and less
// one window, so every window ends before the credits. A
// video too short to leave out the credits uses its whole runtime; one
// shorter than a window gets a single window from 0.
func WindowStarts(durationMs int64) []int64 {
	if durationMs <= 0 {
		return nil
	}
	if durationMs <= WindowMs {
		return []int64{0}
	}
	span := durationMs - max(CreditsMinMs, int64(float64(durationMs)*CreditsShare))
	if span < WindowMs {
		span = durationMs
	}
	out := make([]int64, Windows)
	for i := range int64(Windows) {
		out[i] = (span - WindowMs) * (2*i + 1) / (2 * Windows)
	}
	return out
}
