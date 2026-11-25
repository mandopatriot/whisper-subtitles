package media

import "testing"

func TestWindowStarts(t *testing.T) {
	for _, c := range []struct {
		name       string
		durationMs int64
		creditsMs  int64 // the end no window may reach into
	}{
		// Secret Invasion S01E04: 35:46; its old last window began at 32:47,
		// in the credits, and hung Whisper.
		{"episode", 2_146_000, CreditsMinMs},
		{"2 h 38 min film", 9_480_000, int64(9_480_000 * CreditsShare)},
		{"10 min short", 600_000, CreditsMinMs},
	} {
		s := WindowStarts(c.durationMs)
		if len(s) != Windows {
			t.Fatalf("%s: %d windows, want %d", c.name, len(s), Windows)
		}
		for i, st := range s {
			if st < 0 || (i > 0 && st <= s[i-1]) {
				t.Errorf("%s: window %d starts at %d after %v", c.name, i, st, s[:i])
			}
		}
		if end := s[Windows-1] + WindowMs; end > c.durationMs-c.creditsMs {
			t.Errorf("%s: last window ends at %d, inside the last %d ms", c.name, end, c.creditsMs)
		}
	}
	if s := WindowStarts(2_146_000); s[Windows-1] >= 1_966_805 {
		t.Errorf("S01E04's last window starts at %d, at or after the old credits window (1966805)", s[Windows-1])
	}
	// Too short to leave the credits out: the whole runtime, still inside it.
	s := WindowStarts(200_000)
	if len(s) != Windows || s[Windows-1]+WindowMs > 200_000 || s[0] < 0 {
		t.Errorf("200 s video: %v", s)
	}
	if s := WindowStarts(60_000); len(s) != 1 || s[0] != 0 {
		t.Errorf("60 s video: %v, want one window from 0", s)
	}
	if s := WindowStarts(0); s != nil {
		t.Errorf("no duration: %v", s)
	}
}
