package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func f64(v float64) *float64 { return &v }

// The not-speech rule, at each threshold, with fields missing.
func TestNotSpeech(t *testing.T) {
	if noSpeechProbMin != 0.6 || silentLogprobMax != -1.0 || noSpeechProbAlone != 0.9 {
		t.Fatal("thresholds must be 0.6 with -1.0, and 0.9 alone")
	}
	cases := []struct {
		name     string
		ns, lp   *float64
		dropWant bool
	}{
		{"both missing", nil, nil, false},
		{"combined: both past", f64(0.61), f64(-1.01), true},
		{"combined: no_speech at the bar", f64(0.6), f64(-2), false},
		{"combined: logprob at the bar", f64(0.8), f64(-1.0), false},
		{"combined: logprob just past", f64(0.8), f64(-1.001), true},
		{"no_speech just under the alone bar, logprob fine", f64(0.9), f64(-0.2), false},
		{"no_speech just over the alone bar", f64(0.901), f64(-0.2), true},
		{"no_speech alone, logprob missing, under", f64(0.85), nil, false},
		{"no_speech alone, logprob missing, over", f64(0.95), nil, true},
		{"very low logprob, low no_speech: hard to hear, kept", f64(0.1), f64(-3.5), false},
		{"very low logprob, no_speech missing: kept", nil, f64(-5), false},
	}
	for _, c := range cases {
		if got := notSpeech(Segment{NoSpeechProb: c.ns, AvgLogprob: c.lp}); got != c.dropWant {
			t.Errorf("%s: dropped %t, want %t", c.name, got, c.dropWant)
		}
	}
}

// Values out of range are read as not sent; the answer stays usable.
func TestParseTranscriptConfidence(t *testing.T) {
	body := `{"language":"en","language_probability":1,"video_duration_sec":60,"segments":[
	 {"start":1,"end":2,"text":"a","no_speech_prob":0.2,"avg_logprob":-0.3},
	 {"start":3,"end":4,"text":"b","no_speech_prob":1.5,"avg_logprob":0.4},
	 {"start":5,"end":6,"text":"c"}]}`
	tr, err := parseTranscript([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	s := tr.Segments
	if len(s) != 3 || s[0].NoSpeechProb == nil || *s[0].NoSpeechProb != 0.2 || *s[0].AvgLogprob != -0.3 {
		t.Fatalf("segment 0: %+v", s)
	}
	if s[1].NoSpeechProb != nil || s[1].AvgLogprob != nil {
		t.Errorf("out of range values kept: %v %v", *s[1].NoSpeechProb, *s[1].AvgLogprob)
	}
	if s[2].NoSpeechProb != nil || s[2].AvgLogprob != nil {
		t.Error("missing values read as set")
	}
}

func seg(s, e int64, text string) tSeg { return tSeg{StartMs: s, EndMs: e, Text: text} }

// parseSRTBack reads a built subtitle with the plugin's own reader, and
// keeps each cue's own lines (the reader joins them with a space).
func parseSRTBack(t *testing.T, b []byte) []Cue {
	t.Helper()
	cues, err := readCues(b, "srt")
	if err != nil {
		t.Fatalf("built SRT does not read back: %v\n%s", err, b)
	}
	blocks := strings.Split(strings.TrimSpace(string(b)), "\n\n")
	if len(blocks) != len(cues) {
		t.Fatalf("%d blocks, %d cues", len(blocks), len(cues))
	}
	for i, bl := range blocks {
		lines := strings.Split(bl, "\n")
		if len(lines) < 3 || lines[0] != fmt.Sprint(i+1) {
			t.Fatalf("block %d: %q", i, bl)
		}
		cues[i].Text = strings.Join(lines[2:], "\n")
	}
	return cues
}

func checkCueLimits(t *testing.T, cues []Cue) {
	t.Helper()
	for i, c := range cues {
		lines := strings.Split(c.Text, "\n")
		if len(lines) > cueLines {
			t.Errorf("cue %d has %d lines: %q", i, len(lines), c.Text)
		}
		for _, l := range lines {
			if utf8.RuneCountInString(l) > cueLineRunes {
				t.Errorf("cue %d line over %d: %q", i, cueLineRunes, l)
			}
		}
		if c.EndMs-c.StartMs > cueMaxMs || c.EndMs <= c.StartMs {
			t.Errorf("cue %d runs %d to %d", i, c.StartMs, c.EndMs)
		}
		if i > 0 && c.StartMs < cues[i-1].EndMs {
			t.Errorf("cue %d starts at %d, before cue %d ends at %d", i, c.StartMs, i-1, cues[i-1].EndMs)
		}
	}
}

func TestBuildSRTBasic(t *testing.T) {
	segs := []tSeg{seg(1000, 2500, "  Hello   there. "), seg(2600, 2900, "Hi."), seg(10_000, 12_000, "")}
	b, n, err := buildSRT(segs, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n00:00:01,000 --> 00:00:02,500\nHello there.\n\n2\n00:00:02,600 --> 00:00:03,600\nHi.\n\n"
	if string(b) != want || n != 2 {
		t.Errorf("got %d cues:\n%q\nwant\n%q", n, b, want)
	}
	checkCueLimits(t, parseSRTBack(t, b))
}

// A long segment with word timings is split at the words.
func TestBuildSRTSplitsByWords(t *testing.T) {
	var words []tWord
	var text strings.Builder
	at := int64(5000)
	for i := range 40 {
		w := fmt.Sprintf(" word%02d", i)
		words = append(words, tWord{StartMs: at, EndMs: at + 400, Text: w})
		text.WriteString(w)
		at += 500
	}
	s := tSeg{StartMs: 5000, EndMs: at, Text: text.String(), Words: words}
	b, n, err := buildSRT([]tSeg{s}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	cues := parseSRTBack(t, b)
	if n < 4 || len(cues) != n {
		t.Fatalf("%d cues", n)
	}
	checkCueLimits(t, cues)
	// Each cue starts at one of the words.
	for _, c := range cues {
		first := strings.Fields(c.Text)[0]
		var w tWord
		for _, x := range words {
			if strings.TrimSpace(x.Text) == first {
				w = x
			}
		}
		if w.StartMs != c.StartMs {
			t.Errorf("cue %q starts at %d, its first word at %d", c.Text, c.StartMs, w.StartMs)
		}
	}
	var all []string
	for _, c := range cues {
		all = append(all, strings.Fields(c.Text)...)
	}
	if strings.Join(all, " ") != strings.TrimSpace(text.String()) {
		t.Error("words lost or reordered")
	}
}

// Without word timings, a long segment is split in proportion to its text.
func TestBuildSRTSplitsByChars(t *testing.T) {
	text := strings.Repeat("abcdefghi ", 30) // 300 characters
	b, n, err := buildSRT([]tSeg{seg(0, 20_000, text)}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	cues := parseSRTBack(t, b)
	if n < 4 {
		t.Fatalf("%d cues for 300 characters in 20 s", n)
	}
	checkCueLimits(t, cues)
	if cues[0].StartMs != 0 || cues[len(cues)-1].EndMs != 20_000 {
		t.Errorf("cues run %d to %d, want 0 to 20000", cues[0].StartMs, cues[len(cues)-1].EndMs)
	}
	// Short text over a long time is split by time.
	b, _, err = buildSRT([]tSeg{seg(0, 20_000, "one two three four five six")}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	checkCueLimits(t, parseSRTBack(t, b))
}

// Text with no spaces (Chinese, Japanese) is broken inside the word.
func TestBuildSRTNoSpaces(t *testing.T) {
	text := strings.Repeat("我们", 60) // 120 runes
	b, _, err := buildSRT([]tSeg{seg(0, 6000, text)}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	cues := parseSRTBack(t, b)
	checkCueLimits(t, cues)
	var got strings.Builder
	for _, c := range cues {
		got.WriteString(strings.ReplaceAll(c.Text, "\n", ""))
	}
	if got.String() != text {
		t.Errorf("text changed: %q", got.String())
	}
}

// Two lines balance; the minimum length never overlaps the next line.
func TestBuildSRTWrapAndMinimum(t *testing.T) {
	text := "This line is long enough that it has to be wrapped in two"
	b, _, err := buildSRT([]tSeg{seg(0, 3000, text), seg(3200, 3400, "Short."), seg(3900, 5000, "Next.")}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	cues := parseSRTBack(t, b)
	checkCueLimits(t, cues)
	if lines := strings.Split(cues[0].Text, "\n"); len(lines) != 2 || strings.Join(lines, " ") != text {
		t.Errorf("wrap: %q", cues[0].Text)
	}
	if cues[1].EndMs != 3850 {
		t.Errorf("short cue ends at %d, want 3850 (lengthened, then a gap before the next)", cues[1].EndMs)
	}
}

// Three or more of the same line in a row is a loop: kept once.
func TestBuildSRTLoops(t *testing.T) {
	var segs []tSeg
	for i := range 6 {
		segs = append(segs, seg(int64(i)*2000, int64(i)*2000+1500, "Thank you."))
	}
	segs = append(segs, seg(20_000, 21_000, "No."), seg(22_000, 23_000, "no."), seg(24_000, 25_000, "Fine."))
	_, n, err := buildSRT(segs, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("%d cues, want 4: one Thank you, both No, Fine", n)
	}
}

func TestBuildSRTNoSpeech(t *testing.T) {
	if _, _, err := buildSRT(nil, 60_000); !errors.Is(err, errNoSpeech) {
		t.Errorf("nothing heard: %v", err)
	}
	var segs []tSeg
	for i := range minCuesLong - 1 {
		segs = append(segs, seg(int64(i)*60_000, int64(i)*60_000+1000, fmt.Sprintf("line %d", i)))
	}
	if _, _, err := buildSRT(segs, longVideoMs+1); !errors.Is(err, errNoSpeech) {
		t.Errorf("9 lines in a long video: %v", err)
	}
	if _, _, err := buildSRT(segs, longVideoMs); err != nil {
		t.Errorf("9 lines in a 20 minute video: %v", err)
	}
	segs = append(segs, seg(1_000_000, 1_001_000, "ten"))
	if _, _, err := buildSRT(segs, longVideoMs+1); err != nil {
		t.Errorf("10 lines in a long video: %v", err)
	}
}

func TestBuildSRTSizeCap(t *testing.T) {
	if maxMadeBytes != 8<<20 {
		t.Fatal("the cap must be the site's 8 MB")
	}
	// 30000 lines of 84 three-byte characters: about 9 MB, under the line cap.
	var segs []tSeg
	line := []rune(strings.Repeat("語", 2*cueLineRunes))
	for i := range 30_000 {
		line[0] = rune(0x4e00 + i%2000) // never the same line twice in a row
		segs = append(segs, seg(int64(i)*1100, int64(i)*1100+1000, string(line)))
	}
	if _, _, err := buildSRT(segs, 30_000*1100); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("over 8 MB: %v", err)
	}
}

// A cue of only punctuation is dropped; "you" alone is kept for now, and so
// are numbers and text in any script.
func TestBuildSRTPunctuationOnly(t *testing.T) {
	segs := []tSeg{seg(0, 1000, "."), seg(2000, 3000, "you"), seg(4000, 5000, " ... !? "), seg(6000, 7000, "-"),
		seg(8000, 9000, "♪"), seg(10_000, 11_000, "42"), seg(12_000, 13_000, "¿Qué?"), seg(14_000, 15_000, "我们")}
	b, n, err := buildSRT(segs, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range parseSRTBack(t, b) {
		got = append(got, c.Text)
	}
	if want := "you|42|¿Qué?|我们"; strings.Join(got, "|") != want || n != 4 {
		t.Errorf("kept %q, want %s", got, want)
	}
}
