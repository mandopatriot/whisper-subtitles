package main

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"whisper-subtitles/media"
)

func TestReadCues(t *testing.T) {
	srt := "\xEF\xBB\xBF1\r\n00:00:01,500 --> 00:00:03,000\r\n<i>Hello there</i>\r\nsecond line\r\n\r\n2\n00:01:02.25 --> 00:01:04,000 X1:10\nNext\n\nbroken --> line\n"
	c, err := readCues([]byte(srt), "srt")
	if err != nil || len(c) != 2 || c[0].StartMs != 1500 || c[0].EndMs != 3000 || c[0].Text != "<i>Hello there</i> second line" || c[1].StartMs != 62250 {
		t.Errorf("srt: %+v, %v", c, err)
	}
	vtt := "WEBVTT\n\n01:02.500 --> 01:04.000 align:start\nShort hours\n"
	if c, err := readCues([]byte(vtt), "vtt"); err != nil || len(c) != 1 || c[0].StartMs != 62500 {
		t.Errorf("vtt: %+v, %v", c, err)
	}
	ass := "[Script Info]\nTitle: x\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:05.10,0:00:07.00,Default,,0,0,0,,{\\i1}Hi, you{\\i0}\\Nthere\nComment: 0,0:00:09.00,0:00:10.00,Default,,0,0,0,,not shown\n"
	if c, err := readCues([]byte(ass), "ass"); err != nil || len(c) != 1 || c[0].StartMs != 5100 || c[0].Text != "Hi, you there" {
		t.Errorf("ass: %+v, %v", c, err)
	}
	if _, err := readCues([]byte("nothing timed"), "srt"); err == nil {
		t.Error("a file without cues must be refused")
	}
	if _, err := readCues(make([]byte, maxSubBytes+1), "srt"); err == nil {
		t.Error("a file over 5 MB must be refused")
	}
	if _, err := readCues([]byte("x"), "sub"); err == nil {
		t.Error(".sub must be refused")
	}
}

func TestTokens(t *testing.T) {
	got := tokens("JOHN: [MUSIC PLAYING] I'm <i>going</i> home, (SIGHS) now ♪ -MARY ANN: Okay.")
	want := []string{"i'm", "going", "home", "now", "okay"}
	if len(got) != len(want) {
		t.Errorf("tokens = %v, want %v", got, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("tokens lacks %q: %v", w, got)
		}
	}
}

// lcg is a tiny deterministic random source for the synthetic films.
type lcg uint64

func (l *lcg) next() uint64 {
	*l = *l*6364136223846793005 + 1442695040888963407
	return uint64(*l >> 33)
}
func (l *lcg) float() float64 { return float64(l.next()%1_000_000) / 1_000_000 }

var vocab = strings.Fields(`time people year way day man thing woman life child world school state family student group country problem hand part place case week company system program question work government number night point home water room mother area money story fact month lot right study book eye job word business issue side kind head house service friend father power hour game line end member law car city community name president team minute idea kid body information back parent face others level office door health person art war history party result change morning reason research girl guy moment air teacher force education foot boy age policy process music market sense nation plan college interest death experience effect class control care field development role effort rate heart drug show leader light voice wife police mind price report decision son view relationship town road arm difference value building action model season society tax director position player record paper space ground form event official matter center couple site project activity star table need court oil situation cost industry figure street image phone data picture practice piece land product doctor wall patient worker news test movie north love support technology step baby computer type attention film tree source organization hair window evidence population`)

// film makes cues every 3 to 5 s with unique text for lengthMs.
func film(seed uint64, lengthMs int64) []Cue {
	r := lcg(seed)
	var cues []Cue
	for ms := int64(2000); ms < lengthMs; ms += 3000 + int64(r.next()%2000) {
		n := 4 + int(r.next()%5)
		var w []string
		for range n {
			w = append(w, vocab[r.next()%uint64(len(vocab))])
		}
		cues = append(cues, Cue{StartMs: ms, EndMs: ms + 2500, Text: strings.Join(w, " ")})
	}
	return cues
}

// hear makes the plugin's windows (media.WindowStarts) for speech placed at
// k*cue + b, with phrase timings only: segment starts are rounded to whole
// seconds and moved by up to ±0.6 s; one segment in five merges two cues,
// one in six is misheard (two words changed), and some are fillers ("you").
func hear(cues []Cue, lengthMs int64, k, b float64, lang string, seed uint64) []WindowText {
	return hearAs(cues, lengthMs, k, b, lang, seed, false)
}

// hearWords is hear with word timings: the segments are as rough, but each
// word is timed within ±30 ms of the speech, 300 ms apart.
func hearWords(cues []Cue, lengthMs int64, k, b float64, lang string, seed uint64) []WindowText {
	return hearAs(cues, lengthMs, k, b, lang, seed, true)
}

func hearAs(cues []Cue, lengthMs int64, k, b float64, lang string, seed uint64, words bool) []WindowText {
	r := lcg(seed)
	var wins []WindowText
	for _, base := range media.WindowStarts(lengthMs) {
		w := WindowText{BaseMs: base, Tr: Transcript{Language: lang, Probability: 0.98, Duration: 90, Words: words}}
		for ci := 0; ci < len(cues); ci++ {
			at := k*float64(cues[ci].StartMs) + b - float64(base)
			if at < 0 || at >= 90000 {
				continue
			}
			text := cues[ci].Text
			type spoken struct {
				word string
				at   float64 // seconds into the window
			}
			var said []spoken
			say := func(c Cue) {
				t0 := (k*float64(c.StartMs) + b - float64(base)) / 1000
				for j, f := range strings.Fields(c.Text) {
					said = append(said, spoken{f, t0 + 0.3*float64(j) + r.float()*0.06 - 0.03})
				}
			}
			say(cues[ci])
			if r.next()%5 == 0 && ci+1 < len(cues) {
				text += " " + cues[ci+1].Text
				ci++
				say(cues[ci])
			}
			if r.next()%6 == 0 {
				f := strings.Fields(text)
				f[0], f[len(f)-1] = "squirrel", "mumble"
				text = strings.Join(f, " ")
				said[0].word, said[len(said)-1].word = "squirrel", "mumble"
			}
			start := math.Round(at/1000) + (r.float()*1.2 - 0.6)
			seg := Segment{Start: max(0, start), End: start + 2, Text: text}
			if words {
				for _, sp := range said {
					seg.Words = append(seg.Words, Word{Start: max(0, sp.at), End: max(0, sp.at) + 0.25, Word: " " + sp.word})
				}
			}
			w.Tr.Segments = append(w.Tr.Segments, seg)
			if r.next()%7 == 0 {
				filler := Segment{Start: start + 2, End: start + 3, Text: "you"}
				if words {
					filler.Words = []Word{{Start: start + 2, End: start + 2.2, Word: " you"}}
				}
				w.Tr.Segments = append(w.Tr.Segments, filler)
			}
		}
		wins = append(wins, w)
	}
	return wins
}

const filmMs = 100 * 60_000

// applyFix is HolyArrs's subtext Fix.apply, for checking a result.
func applyFix(ms int64, r FitResult) int64 {
	if r.FromFPS != 0 {
		ms = int64(float64(ms)*r.FromFPS/r.ToFPS + 0.5)
	}
	return max(ms+r.OffsetMs, 0)
}

func TestFitOffsets(t *testing.T) {
	cues := film(1, filmMs)
	for _, c := range []struct {
		name string
		b    float64 // speech = cue + b, so the fix is +b
		sync bool
	}{
		{"subtitle 2.5 s early", 2500, true},
		{"subtitle 7.34 s late", -7340, true},
		{"subtitle 9 min early", 540_000, true},
		{"in sync", 0, false},
		{"900 ms off: under the 1000 ms phrase minimum", 900, false},
		{"1100 ms off: over the phrase minimum", 1100, true},
	} {
		r := fitSubtitle(cues, "en", hear(cues, filmMs, 1, c.b, "en", 7))
		if r.Timing != "phrases" {
			t.Errorf("%s: timing %q, want phrases", c.name, r.Timing)
		}
		if r.Sync != c.sync {
			t.Errorf("%s: %s", c.name, r)
			continue
		}
		// Within 150 ms: over 20 seeds the simulated phrase noise gave
		// errors up to 110 ms (medians -20 to +35 ms).
		if c.sync && (math.Abs(float64(r.OffsetMs)-c.b) > 150 || r.FromFPS != 0) {
			t.Errorf("%s: %s; want offset %+.0f ms within 150", c.name, r, c.b)
		}
	}
}

func TestFitFrameRates(t *testing.T) {
	cues := film(2, filmMs)
	// A subtitle timed for 25 fps on a 23.976 video: speech = cue*25/23.976.
	k := 25 / (24000.0 / 1001)
	r := fitSubtitle(cues, "en", hear(cues, filmMs, k, 1200, "en", 3))
	if !r.Sync || r.FromFPS != 25 || r.ToFPS != 23.976 || math.Abs(float64(r.OffsetMs)-1200) > 100 {
		t.Fatalf("25 to 23.976 plus 1.2 s: %s", r)
	}
	// HolyArrs's own formula, applied to the result, lands the cues on the speech.
	for _, c := range cues[100:110] {
		if d := float64(applyFix(c.StartMs, r)) - (k*float64(c.StartMs) + 1200); math.Abs(d) > 150 {
			t.Errorf("cue at %d lands %.0f ms from the speech", c.StartMs, d)
		}
	}
	// The other way: a 23.976 subtitle on a 25 fps video.
	r = fitSubtitle(cues, "en", hear(cues, filmMs, 1/k, 0, "en", 4))
	if !r.Sync || r.FromFPS != 23.976 || r.ToFPS != 25 || math.Abs(float64(r.OffsetMs)) > 100 {
		t.Errorf("23.976 to 25: %s", r)
	}
}

func TestFitRefusals(t *testing.T) {
	cues := film(3, filmMs)
	cases := []struct {
		name string
		wins []WindowText
		rule string
	}{
		{"other language", hear(cues, filmMs, 1, 2500, "es", 1), "another language"},
		{"low language probability", func() []WindowText {
			w := hear(cues, filmMs, 1, 2500, "en", 1)
			for i := range w {
				w[i].Tr.Probability = 0.79
			}
			return w
		}(), "another language"},
		{"3 of 12 windows in the language", func() []WindowText {
			w := hear(cues, filmMs, 1, 2500, "en", 1)
			for i := 3; i < len(w); i++ {
				w[i].Tr.Language = "de"
			}
			return w
		}(), "only 3 of 12"},
		{"split windows", func() []WindowText {
			a, b := hear(cues, filmMs, 1, 2000, "en", 1), hear(cues, filmMs, 1, -5000, "en", 1)
			return append(a[:6:6], b[6:]...)
		}(), "agree"},
		{"no matching text", hear(film(99, filmMs), filmMs, 1, 2500, "en", 1), "matched lines"},
		{"failed uploads", func() []WindowText {
			w := hear(cues, filmMs, 1, 2500, "en", 1)
			for i := range 9 {
				w[i].Err = fmt.Errorf("503")
			}
			return w
		}(), "only 3 of 12"},
	}
	for _, c := range cases {
		r := fitSubtitle(cues, "en", c.wins)
		if r.Sync || !strings.Contains(r.Reason, c.rule) {
			t.Errorf("%s: %s; want refused naming %q", c.name, r, c.rule)
		}
	}
}

func TestFitNoisyWindows(t *testing.T) {
	cues := film(4, filmMs)
	w := hear(cues, filmMs, 1, 2500, "en", 5)
	// Three windows hear another film in the same language.
	other := hear(film(77, filmMs), filmMs, 1, 0, "en", 5)
	w[1], w[4], w[6] = other[1], other[4], other[6]
	r := fitSubtitle(cues, "en", w)
	if !r.Sync || math.Abs(float64(r.OffsetMs)-2500) > 100 {
		t.Errorf("3 of 12 windows wrong: %s", r)
	}
}

func TestFitRepeatedLineIsNoAnchor(t *testing.T) {
	cues := []Cue{{StartMs: 10_000, Text: "get down get down now"}, {StartMs: 50_000, Text: "get down get down now"}}
	wins := []WindowText{{BaseMs: 0, Tr: Transcript{Language: "en", Probability: 1, Segments: []Segment{{Start: 12, Text: "get down get down now"}}}}}
	if a := anchorsFor(candidates(cues), wins, []bool{true}, 1, false); len(a) != 0 {
		t.Errorf("a line said twice anchored: %+v", a)
	}
	// Two cues heard as one segment still anchor, at the first cue.
	cues = []Cue{{StartMs: 10_000, Text: "where is the car"}, {StartMs: 12_000, Text: "in the old garage"}, {StartMs: 30_000, Text: "something else entirely here"}}
	wins[0].Tr.Segments = []Segment{{Start: 11, Text: "Where is the car? In the old garage."}}
	if a := anchorsFor(candidates(cues), wins, []bool{true}, 1, false); len(a) != 1 || a[0].d != 1000 {
		t.Errorf("merged cues: %+v", a)
	}
}

func TestFitFPSNeedsAdvantage(t *testing.T) {
	// With only 5 usable windows a frame-rate change can't reach the 6
	// agreeing windows it needs, so it is refused.
	cues := film(5, filmMs)
	w := hear(cues, filmMs, 25/(24000.0/1001), 0, "en", 6)
	for i := 0; i < 7; i++ {
		w[i].Tr.Segments = nil
	}
	r := fitSubtitle(cues, "en", w)
	if r.Sync {
		t.Errorf("frame-rate change from 5 windows: %s", r)
	}
}

func TestFitNegativeCues(t *testing.T) {
	cues := film(6, filmMs)
	// Speech 3 s before the subtitle: the first cue (at 2 s) would go below 0.
	early := []Cue{{StartMs: 100, Text: "a"}, {StartMs: 500, Text: "b"}, {StartMs: 900, Text: "c"}}
	all := append(early, cues...)
	r := fitSubtitle(all, "en", hear(cues, filmMs, 1, -3000, "en", 8))
	if r.Sync || !strings.Contains(r.Reason, "before the start") {
		t.Errorf("three cues pushed below 0: %s", r)
	}
}

// With word timings the offset is exact although the segments are as rough
// as phrase timings, and the 300 ms minimum applies.
func TestFitWordTimings(t *testing.T) {
	cues := film(11, filmMs)
	for _, c := range []struct {
		name string
		b    float64
		sync bool
	}{
		{"2.5 s early", 2500, true},
		{"7.34 s late", -7340, true},
		{"400 ms off: over the 300 ms word minimum", 400, true},
		{"250 ms off: under it", 250, false},
		{"in sync", 0, false},
	} {
		r := fitSubtitle(cues, "en", hearWords(cues, filmMs, 1, c.b, "en", 12))
		if r.Timing != "words" || r.Sync != c.sync {
			t.Errorf("%s: %s", c.name, r)
			continue
		}
		if math.Abs(float64(r.OffsetMs)-c.b) > 30 {
			t.Errorf("%s: offset %d ms, want %+.0f within 30", c.name, r.OffsetMs, c.b)
		}
	}
	// 25 to 23.976 is found with words too.
	k := 25 / (24000.0 / 1001)
	r := fitSubtitle(cues, "en", hearWords(cues, filmMs, k, -120, "en", 13))
	if !r.Sync || r.FromFPS != 25 || r.ToFPS != 23.976 || math.Abs(float64(r.OffsetMs)+120) > 30 {
		t.Errorf("words, 25 to 23.976 and -120 ms: %s", r)
	}
	// One window that heard speech without words: phrases for all, and
	// 900 ms is then under the phrase minimum.
	w := hearWords(cues, filmMs, 1, 900, "en", 14)
	w[5].Tr.Words = false
	for i := range w[5].Tr.Segments {
		w[5].Tr.Segments[i].Words = nil
	}
	if r := fitSubtitle(cues, "en", w); r.Timing != "phrases" || r.Sync {
		t.Errorf("mixed windows: %s; want phrases and no sync", r)
	}
	// A window with no speech doesn't decide the timing.
	w = hearWords(cues, filmMs, 1, 900, "en", 14)
	w[5].Tr = Transcript{Language: "en", Probability: 0.98}
	if r := fitSubtitle(cues, "en", w); r.Timing != "words" || !r.Sync {
		t.Errorf("one silent window: %s; want words and a sync", r)
	}
}

// fitInlierTolMs at its edge: 8 windows at +2000 and 4 more 560 ms away all
// agree; 640 ms away the 4 don't, which leaves 8 of 12 (under 75%).
func TestFitInlierTolerance(t *testing.T) {
	cues := film(15, filmMs)
	mix := func(d float64) FitResult {
		a, b := hearWords(cues, filmMs, 1, 2000, "en", 16), hearWords(cues, filmMs, 1, 2000+d, "en", 16)
		for _, i := range []int{1, 4, 7, 10} {
			a[i] = b[i]
		}
		return fitSubtitle(cues, "en", a)
	}
	if r := mix(560); !r.Sync || r.Inliers != 12 {
		t.Errorf("560 ms apart: %s; want 12 agreeing and a sync", r)
	}
	if r := mix(640); r.Sync || r.Inliers != 8 || !strings.Contains(r.Reason, "agree") {
		t.Errorf("640 ms apart: %s; want 8 agreeing and refused", r)
	}
}

func TestLeadWord(t *testing.T) {
	ws := func(s string) []Word {
		var out []Word
		for i, f := range strings.Fields(s) {
			out = append(out, Word{Start: float64(i), Word: " " + f})
		}
		return out
	}
	for _, c := range []struct {
		words, cue string
		at         float64
		ok         bool
	}{
		{"where is the car", "Where is the car?", 0, true},
		{"yes you know you said so", "You said so.", 3, true},      // not the first "you"
		{"okay then. Where is the car", "- Where is it?", 2, true}, // two of three lead words
		{"squirrel is the car", "Where is the car?", 0, false},     // first word misheard
		{"where now", "Where is the car?", 0, false},               // one lead word only
		{"go", "Go!", 0, true},                                     // a one-word cue needs only it
	} {
		lead := tokenList(c.cue)
		w, ok := leadWord(ws(c.words), lead[:min(len(lead), fitLeadTokens)])
		if ok != c.ok || (ok && w.Start != c.at) {
			t.Errorf("%q in %q: %+v %t; want at %v %t", c.cue, c.words, w, ok, c.at, c.ok)
		}
	}
}
