package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Whisper's own "not speech" signals. A segment is dropped only when the
// server says it is likely not speech: by no_speech_prob together with a low
// avg_logprob (Whisper's own silence rule), or by no_speech_prob alone when
// it is very high. A low avg_logprob alone never drops a segment: it can be
// real speech that is hard to hear.
const (
	noSpeechProbMin   = 0.6  // combined rule: no_speech_prob above this
	silentLogprobMax  = -1.0 // and avg_logprob below this
	noSpeechProbAlone = 0.9  // no_speech_prob alone above this
)

// Cue limits for a written subtitle.
const (
	cueLineRunes = 42
	cueLines     = 2
	cueMaxMs     = 7_000
	cueMinMs     = 1_000
	// cueGapMs is kept between a cue and the next: HolyArrs's subtext.MinGapMS,
	// two frames at 23.976 fps, so its overlap cleanup has nothing to change.
	cueGapMs = 84
	// loopRun is how many times the same line in a row counts as Whisper
	// looping; only the first is kept. Two in a row ("No. No.") stay.
	loopRun = 3
	// A video longer than longVideoMs with fewer than minCuesLong cues has
	// almost no speech heard: no subtitle is made.
	longVideoMs = 20 * 60_000
	minCuesLong = 10
	maxMadeCues = 50_000
	// maxMadeBytes is the subtitle site's cap on one file (hooks.subtitleMax).
	maxMadeBytes = 8 << 20
)

// tWord is one word, in ms of the video.
type tWord struct {
	StartMs int64  `json:"s"`
	EndMs   int64  `json:"e"`
	Text    string `json:"t"`
}

// tSeg is one stretch of speech kept for the subtitle, in ms of the video.
type tSeg struct {
	StartMs int64   `json:"s"`
	EndMs   int64   `json:"e"`
	Text    string  `json:"t"`
	Words   []tWord `json:"w,omitempty"`
}

// notSpeech says whether Whisper marked s as likely not speech.
func notSpeech(s Segment) bool {
	if s.NoSpeechProb == nil {
		return false
	}
	p := *s.NoSpeechProb
	if p > noSpeechProbAlone {
		return true
	}
	return s.AvgLogprob != nil && p > noSpeechProbMin && *s.AvgLogprob < silentLogprobMax
}

// errNoSpeech is a make that heard too little to write a subtitle.
var errNoSpeech = errors.New("heard almost no speech")

// hasWords says whether text has a letter or a digit. A line of only
// punctuation, such as ".", is something Whisper made up at a piece's edge.
func hasWords(text string) bool {
	return strings.IndexFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) >= 0
}

// cleanText trims a line and collapses its spaces.
func cleanText(s string) string { return strings.Join(strings.Fields(s), " ") }

// tok is a piece of a line: a word, or part of one too long for a line.
// glue means no space before it.
type tok struct {
	s    string
	glue bool
}

func toks(text string) []tok {
	var out []tok
	for _, w := range strings.Fields(text) {
		first := true
		for utf8.RuneCountInString(w) > cueLineRunes {
			r := []rune(w)
			out = append(out, tok{string(r[:cueLineRunes]), !first})
			w, first = string(r[cueLineRunes:]), false
		}
		out = append(out, tok{w, !first})
	}
	return out
}

func joinToks(ts []tok) string {
	var b strings.Builder
	for i, t := range ts {
		if i > 0 && !t.glue {
			b.WriteByte(' ')
		}
		b.WriteString(t.s)
	}
	return b.String()
}

func runes(ts []tok) int { return utf8.RuneCountInString(joinToks(ts)) }

// wrap breaks a line into display lines of at most cueLineRunes: as two
// balanced lines when it can, else greedily.
func wrap(ts []tok) []string {
	if len(ts) == 0 {
		return nil
	}
	if runes(ts) <= cueLineRunes {
		return []string{joinToks(ts)}
	}
	best, bestLen := -1, math.MaxInt
	for k := 1; k < len(ts); k++ {
		a, b := runes(ts[:k]), runes(ts[k:])
		if a <= cueLineRunes && b <= cueLineRunes && max(a, b) < bestLen {
			best, bestLen = k, max(a, b)
		}
	}
	if best > 0 {
		return []string{joinToks(ts[:best]), joinToks(ts[best:])}
	}
	var lines []string
	var cur []tok
	for _, t := range ts {
		if len(cur) > 0 && runes(append(cur[:len(cur):len(cur)], t)) > cueLineRunes {
			lines = append(lines, joinToks(cur))
			cur = nil
		}
		cur = append(cur, t)
	}
	return append(lines, joinToks(cur))
}

func fits(ts []tok) bool { return len(wrap(ts)) <= cueLines }

// segCues turns one segment into cues that each fit two lines and seven
// seconds: split at word timings when the server sent them, else in
// proportion to the characters.
func segCues(s tSeg) []Cue {
	text := cleanText(s.Text)
	if !hasWords(text) || s.EndMs <= s.StartMs {
		return nil
	}
	ts := toks(text)
	if fits(ts) && s.EndMs-s.StartMs <= cueMaxMs {
		return []Cue{{StartMs: s.StartMs, EndMs: s.EndMs, Text: strings.Join(wrap(ts), "\n")}}
	}
	if c := cuesByWords(s.Words); len(c) > 0 {
		return c
	}
	return cuesByChars(s, ts)
}

func cuesByWords(words []tWord) []Cue {
	ws := make([]tWord, 0, len(words))
	for _, w := range words {
		if strings.TrimSpace(w.Text) != "" && w.EndMs >= w.StartMs {
			ws = append(ws, w)
		}
	}
	if len(ws) == 0 {
		return nil
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].StartMs < ws[j].StartMs })
	var out []Cue
	var raw strings.Builder
	var first, last tWord
	n := 0
	flush := func() {
		if n > 0 {
			out = append(out, Cue{StartMs: first.StartMs, EndMs: last.EndMs, Text: strings.Join(wrap(toks(cleanText(raw.String()))), "\n")})
		}
		raw.Reset()
		n = 0
	}
	for _, w := range ws {
		if n > 0 {
			// Words carry their own leading space, so they are joined as sent.
			next := raw.String() + w.Text
			if !fits(toks(cleanText(next))) || w.EndMs-first.StartMs > cueMaxMs {
				flush()
			}
		}
		if n == 0 {
			first = w
		}
		raw.WriteString(w.Text)
		last = w
		n++
	}
	flush()
	return out
}

func cuesByChars(s tSeg, ts []tok) []Cue {
	total := runes(ts)
	dur := s.EndMs - s.StartMs
	n := max(int(math.Ceil(float64(total)/float64(cueLineRunes*cueLines))), int(math.Ceil(float64(dur)/cueMaxMs)), 1)
	target := int(math.Ceil(float64(total) / float64(n)))
	var pieces [][]tok
	var cur []tok
	for _, t := range ts {
		if len(cur) > 0 && (runes(cur) >= target || !fits(append(cur[:len(cur):len(cur)], t))) {
			pieces = append(pieces, cur)
			cur = nil
		}
		cur = append(cur, t)
	}
	pieces = append(pieces, cur)
	sum := 0
	for _, p := range pieces {
		sum += runes(p)
	}
	out := make([]Cue, 0, len(pieces))
	at, done := s.StartMs, 0
	for i, p := range pieces {
		done += runes(p)
		end := s.StartMs + dur*int64(done)/int64(sum)
		if i == len(pieces)-1 {
			end = s.EndMs
		}
		out = append(out, Cue{StartMs: at, EndMs: end, Text: strings.Join(wrap(p), "\n")})
		at = end
	}
	return out
}

// collapseLoops keeps only the first of loopRun or more segments in a row
// with the same text.
func collapseLoops(segs []tSeg) []tSeg {
	norm := func(s string) string { return strings.ToLower(cleanText(s)) }
	out := make([]tSeg, 0, len(segs))
	for i := 0; i < len(segs); {
		j := i + 1
		for j < len(segs) && norm(segs[j].Text) == norm(segs[i].Text) {
			j++
		}
		if j-i >= loopRun {
			out = append(out, segs[i])
		} else {
			out = append(out, segs[i:j]...)
		}
		i = j
	}
	return out
}

// buildSRT writes the kept speech of a video of videoMs as an SRT file.
func buildSRT(segs []tSeg, videoMs int64) ([]byte, int, error) {
	segs = collapseLoops(segs)
	var cues []Cue
	for _, s := range segs {
		cues = append(cues, segCues(s)...)
		if len(cues) > maxMadeCues {
			return nil, 0, fmt.Errorf("over %d lines", maxMadeCues)
		}
	}
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].StartMs < cues[j].StartMs })
	for i := range cues {
		if i > 0 && cues[i].StartMs <= cues[i-1].StartMs {
			cues[i].StartMs = cues[i-1].StartMs + 1
		}
		if cues[i].EndMs < cues[i].StartMs+cueMinMs {
			cues[i].EndMs = cues[i].StartMs + cueMinMs
		}
		cues[i].EndMs = min(cues[i].EndMs, cues[i].StartMs+cueMaxMs)
	}
	// Never overlap the next line: end a gap before it when there is room,
	// else where it starts.
	for i := 0; i+1 < len(cues); i++ {
		next := cues[i+1].StartMs
		if cues[i].EndMs > next-cueGapMs {
			if next-cueGapMs > cues[i].StartMs {
				cues[i].EndMs = next - cueGapMs
			} else {
				cues[i].EndMs = next
			}
		}
	}
	if len(cues) == 0 || (videoMs > longVideoMs && len(cues) < minCuesLong) {
		return nil, len(cues), errNoSpeech
	}
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtStamp(c.StartMs), srtStamp(c.EndMs), c.Text)
		if b.Len() > maxMadeBytes {
			return nil, len(cues), fmt.Errorf("the subtitle is over %d bytes", maxMadeBytes)
		}
	}
	return []byte(b.String()), len(cues), nil
}

func srtStamp(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}
