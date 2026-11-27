package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"whisper-subtitles/media"
)

// Piece lengths compared, and the stretch of each film used.
var evalPieces = []int64{30_000, 60_000, 90_000, 600_000}

const (
	evalMs      = 600_000 // the first 10 minutes are scored
	evalRefMs   = 630_000 // the reference reaches past them, for lines at the end
	evalRefPc   = 120_000 // reference pieces on the word-timing server
	evalNearMs  = 10_000  // how far apart text may be and still count as the same
	evalCovered = 0.5     // share of a line's words that must be found
)

// evalRun is one transcription, kept so a rerun only scores.
type evalRun struct {
	Seconds float64
	SRT     string
	Err     string
}

// TestPieceLengths compares upload lengths on real films (see the function
// body for its variables). For each film in $WHISPER_SUBTITLES_PIECES_FILES
// (paths inside $WHISPER_SUBTITLES_LIB, separated by |), it writes the first 10
// minutes as subtitles with each piece length on $WHISPER_SUBTITLES_ASR_URL,
// and scores them against word timings from $WHISPER_SUBTITLES_REF_URL. One
// server is busy at a time. Runs and references are cached in
// $WHISPER_SUBTITLES_PIECES_OUT (never inside the library or a folder in
// $WHISPER_SUBTITLES_PROTECT).
func TestPieceLengths(t *testing.T) {
	lib := testLibrary(t)
	files, asr, ref, out := os.Getenv("WHISPER_SUBTITLES_PIECES_FILES"), os.Getenv("WHISPER_SUBTITLES_ASR_URL"),
		os.Getenv("WHISPER_SUBTITLES_REF_URL"), os.Getenv("WHISPER_SUBTITLES_PIECES_OUT")
	if files == "" || asr == "" || ref == "" || out == "" {
		t.Skip("set WHISPER_SUBTITLES_PIECES_FILES, WHISPER_SUBTITLES_ASR_URL, WHISPER_SUBTITLES_REF_URL and WHISPER_SUBTITLES_PIECES_OUT")
	}
	writableOut(t, out, lib)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var report strings.Builder
	fmt.Fprintf(&report, "| Film | Piece | Lines (ref) | Missed | False cues | Starts measured | Median start error | Worst start error | Time per 10 min |\n|---|---|---|---|---|---|---|---|---|\n")
	var details strings.Builder
	for _, rel := range strings.Split(files, "|") {
		name := strings.SplitN(filepath.Base(rel), " [", 2)[0]
		full, _, err := resolveVideo([]Folder{{"lib", lib}}, "lib", rel)
		if err != nil {
			t.Fatal(err)
		}
		refSegs := evalReference(t, ctx, full, ref, filepath.Join(out, name+".ref.json"))
		for _, pc := range evalPieces {
			run := evalTranscribe(t, ctx, full, asr, pc, filepath.Join(out, fmt.Sprintf("%s.%ds.json", name, pc/1000)))
			if run.Err != "" {
				fmt.Fprintf(&report, "| %s | %s | | failed: %s | | | | | |\n", name, pieceName(pc), run.Err)
				continue
			}
			cues, err := readCues([]byte(run.SRT), "srt")
			if err != nil {
				t.Fatal(err)
			}
			s := score(refSegs, cues)
			fmt.Fprintf(&report, "| %s | %s | %d | %d | %d | %d | %.2f s | %.2f s | %.0f s |\n", name, pieceName(pc), s.lines, len(s.missed), len(s.falses),
				s.measured, s.median/1000, s.worst/1000, run.Seconds)
			fmt.Fprintf(&details, "\n### %s, %s\n\nFalse cues: %s\n\nMissed: %s\n", name, pieceName(pc), list(s.falses), list(s.missed))
		}
	}
	all := report.String() + details.String()
	if err := os.WriteFile(filepath.Join(out, "report.md"), []byte(all), 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Println(all)
}

func pieceName(ms int64) string {
	if ms >= 60_000 && ms%60_000 == 0 && ms > 90_000 {
		return fmt.Sprintf("%d min", ms/60_000)
	}
	return fmt.Sprintf("%d s", ms/1000)
}

func list(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, "; ")
}

func openShort(ms int64) func(string) (video, io.Closer, error) {
	return func(p string) (video, io.Closer, error) {
		v, c, err := openVideo(p)
		if err != nil {
			return nil, nil, err
		}
		return shortVideo{v, ms}, c, nil
	}
}

// evalReference transcribes the film's start on the word-timing server in
// evalRefPc pieces, keeping the words.
func evalReference(t *testing.T, ctx context.Context, full, url, cache string) []tSeg {
	t.Helper()
	var segs []tSeg
	if b, err := os.ReadFile(cache); err == nil && json.Unmarshal(b, &segs) == nil && len(segs) > 0 {
		return segs
	}
	v, c, err := openShort(evalRefMs)(full)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ch, err := media.PickAudio(v.AllTracks(), "en")
	if err != nil || ch.Untagged {
		t.Fatalf("no English track: %v", err)
	}
	m := &maker{dataDir: t.TempDir(), asr: newWhisper(url), queue: newASRQueue(), logf: t.Logf, pieceMs: evalRefPc}
	for start := int64(0); start < v.Duration(); {
		cf, err := m.chunk(ctx, v, ch.Track, "en", start, v.Duration(), segs)
		if err != nil {
			t.Fatalf("reference at %d: %v", start, err)
		}
		segs = append(segs, cf.Segs...)
		start = cf.Next
	}
	for _, s := range segs {
		if len(s.Words) == 0 {
			t.Fatalf("the reference server sent no word timings (segment at %d)", s.StartMs)
		}
	}
	b, _ := json.Marshal(segs)
	if err := os.WriteFile(cache, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return segs
}

// evalTranscribe writes the film's first 10 minutes as a make does, with
// pieces of pc.
func evalTranscribe(t *testing.T, ctx context.Context, full, url string, pc int64, cache string) evalRun {
	t.Helper()
	var run evalRun
	if b, err := os.ReadFile(cache); err == nil && json.Unmarshal(b, &run) == nil && (run.SRT != "" || run.Err != "") {
		return run
	}
	dir, rel := filepath.Split(full)
	data := t.TempDir()
	open := openShort(evalMs)
	v, c, err := open(full)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := media.PickAudio(v.AllTracks(), "en")
	c.Close()
	if err != nil || ch.Untagged {
		t.Fatalf("no English track: %v", err)
	}
	fi, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	k := offerKey{Track: ch.Track.Number, Lang: "en", Size: fi.Size(), MTime: fi.ModTime().Unix()}
	m := &maker{dataDir: data, folders: []Folder{{"f", dir}}, open: open, asr: newWhisper(url), queue: newASRQueue(),
		cache: newMadeCache(data), logf: t.Logf, pieceMs: pc}
	t0 := time.Now()
	body, err := m.make(ctx, makeReq{Key: k.String(), Language: "en", FileID: 1, Folder: "f", Path: rel})
	run.Seconds = time.Since(t0).Seconds()
	run.SRT = string(body)
	if err != nil {
		run.Err = err.Error()
	}
	b, _ := json.Marshal(run)
	if err := os.WriteFile(cache, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return run
}

// normTokens lowercases text and keeps runs of letters and digits.
func normTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

// covered is the share of want found in have, counting repeats.
func covered(want, have []string) float64 {
	if len(want) == 0 {
		return 1
	}
	bag := map[string]int{}
	for _, h := range have {
		bag[h]++
	}
	n := 0
	for _, w := range want {
		if bag[w] > 0 {
			bag[w]--
			n++
		}
	}
	return float64(n) / float64(len(want))
}

type evalScore struct {
	lines, measured int
	missed, falses  []string
	median, worst   float64 // ms
}

func stamp(ms int64) string {
	d := (ms + 50) / 100 // tenths, rounded
	return fmt.Sprintf("%d:%02d.%d", d/600, d/10%60, d%10)
}

// score compares cues with the reference: a reference line is missed when
// under half its words are in cues within 10 s of it; a cue is false when
// under half its words were said within 10 s of it; a cue's start error is
// its start minus the reference time of its first word, found near it.
func score(ref []tSeg, cues []Cue) evalScore {
	var s evalScore
	type rw struct {
		ms  int64
		tok string
	}
	var words []rw
	for _, seg := range ref {
		for _, w := range seg.Words {
			for _, tok := range normTokens(w.Text) {
				words = append(words, rw{w.StartMs, tok})
			}
		}
	}
	for _, seg := range ref {
		toks := normTokens(seg.Text)
		if seg.StartMs >= evalMs || len(toks) == 0 {
			continue
		}
		s.lines++
		var have []string
		for _, c := range cues {
			if c.StartMs >= seg.StartMs-evalNearMs && c.StartMs <= seg.EndMs+evalNearMs {
				have = append(have, normTokens(c.Text)...)
			}
		}
		if covered(toks, have) < evalCovered {
			s.missed = append(s.missed, fmt.Sprintf("%s %q", stamp(seg.StartMs), seg.Text))
		}
	}
	var errs []float64
	for _, c := range cues {
		toks := normTokens(c.Text)
		if c.StartMs >= evalMs || len(toks) == 0 {
			continue
		}
		var said []string
		for _, w := range words {
			if w.ms >= c.StartMs-evalNearMs && w.ms <= c.EndMs+evalNearMs {
				said = append(said, w.tok)
			}
		}
		if covered(toks, said) < evalCovered {
			s.falses = append(s.falses, fmt.Sprintf("%s %q", stamp(c.StartMs), strings.ReplaceAll(c.Text, "\n", " ")))
			continue
		}
		best := math.Inf(1)
		for i, w := range words {
			if w.tok != toks[0] || math.Abs(float64(c.StartMs-w.ms)) > evalNearMs {
				continue
			}
			// With two words or more, the next word must match too.
			if len(toks) > 1 && (i+1 >= len(words) || words[i+1].tok != toks[1]) {
				continue
			}
			if d := float64(c.StartMs - w.ms); math.Abs(d) < math.Abs(best) {
				best = d
			}
		}
		if !math.IsInf(best, 1) {
			errs = append(errs, math.Abs(best))
		}
	}
	s.measured = len(errs)
	if len(errs) > 0 {
		sort.Float64s(errs)
		n := len(errs)
		s.median, s.worst = (errs[(n-1)/2]+errs[n/2])/2, errs[n-1]
	}
	return s
}

// The scoring itself, on made-up lines.
func TestScorePieces(t *testing.T) {
	w := func(ms int64, s string) tWord { return tWord{StartMs: ms, EndMs: ms + 300, Text: " " + s} }
	ref := []tSeg{
		{StartMs: 10_000, EndMs: 12_000, Text: "Okay, here we go.", Words: []tWord{w(10_000, "Okay,"), w(10_500, "here"), w(11_000, "we"), w(11_500, "go.")}},
		{StartMs: 40_000, EndMs: 41_000, Text: "Speed.", Words: []tWord{w(40_000, "Speed.")}},
		{StartMs: 80_000, EndMs: 82_000, Text: "I am lightning.", Words: []tWord{w(80_000, "I"), w(80_300, "am"), w(80_600, "lightning.")}},
	}
	cues := []Cue{
		{StartMs: 9_000, EndMs: 12_000, Text: "Okay, here we go."}, // 1 s early
		{StartMs: 30_000, EndMs: 31_000, Text: "you"},              // false
		{StartMs: 40_200, EndMs: 41_000, Text: "Speed."},           // 0.2 s late
	}
	s := score(ref, cues)
	if s.lines != 3 || len(s.missed) != 1 || !strings.Contains(s.missed[0], "lightning") {
		t.Errorf("lines %d, missed %v", s.lines, s.missed)
	}
	if len(s.falses) != 1 || !strings.Contains(s.falses[0], "you") {
		t.Errorf("false %v", s.falses)
	}
	if s.measured != 2 || s.median != 600 || s.worst != 1000 {
		t.Errorf("measured %d, median %v, worst %v", s.measured, s.median, s.worst)
	}
}
