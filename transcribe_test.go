package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"whisper-subtitles/media"
)

// speechEvery is how often the fake film's speaker says a line, and
// speechLen how long each takes.
const (
	speechEvery = 4000
	speechLen   = 2500
)

// filmAV is a fake film and Whisper server for makes: line n is said from
// n*speechEvery, and Whisper hears every line inside the window it is sent.
type filmAV struct {
	dur    int64
	tracks []media.Track
	capMs  int64 // a window stops after this much audio (the 64 MiB cap)

	mu     sync.Mutex
	last   media.Window
	reads  []struct{ from, dur int64 }
	langs  []string
	calls  int
	onCall func(n int) error
	// extra adds segments to what Whisper hears in a window.
	extra func(w media.Window) []Segment
}

func newFilmAV(dur int64) *filmAV {
	return &filmAV{dur: dur, tracks: []media.Track{engTrack()}}
}

func (f *filmAV) Duration() int64          { return f.dur }
func (f *filmAV) AllTracks() []media.Track { return f.tracks }
func (f *filmAV) Extract(t media.Track, from, dur, max int64, w io.Writer) (media.Window, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, struct{ from, dur int64 }{from, dur})
	end := min(from+dur, f.dur)
	win := media.Window{BaseMs: from, EndMs: end, Blocks: 10}
	if f.capMs > 0 && end-from > f.capMs {
		win.EndMs, win.Capped = from+f.capMs, true
	}
	n, err := w.Write([]byte("audio"))
	win.Bytes = int64(n)
	f.last = win
	return win, err
}

func (f *filmAV) TranscribeChunk(ctx context.Context, path, lang string) (Transcript, error) {
	f.mu.Lock()
	f.calls++
	n, win, hook, extra := f.calls, f.last, f.onCall, f.extra
	f.langs = append(f.langs, lang)
	f.mu.Unlock()
	if _, err := os.Stat(path); err != nil {
		return Transcript{}, fmt.Errorf("chunk file missing during upload: %v", err)
	}
	if hook != nil {
		if err := hook(n); err != nil {
			return Transcript{}, err
		}
	}
	tr := Transcript{Language: lang, Probability: 1, Duration: float64(win.EndMs-win.BaseMs) / 1000}
	sec := func(ms int64) float64 { return float64(ms-win.BaseMs) / 1000 }
	for i := win.BaseMs / speechEvery; i*speechEvery < win.EndMs; i++ {
		st, en := i*speechEvery, i*speechEvery+speechLen
		if st < win.BaseMs || en > win.EndMs {
			continue // cut by the window's edge
		}
		text := fmt.Sprintf(" Line %d is said here.", i)
		tr.Segments = append(tr.Segments, Segment{Start: sec(st), End: sec(en), Text: text,
			Words: []Word{{Start: sec(st), End: sec(st + 1000), Word: fmt.Sprintf(" Line %d", i)}, {Start: sec(st + 1000), End: sec(en), Word: " is said here."}}})
	}
	if extra != nil {
		tr.Segments = append(tr.Segments, extra(win)...)
	}
	return tr, nil
}

type makeRig struct {
	m   *maker
	av  *filmAV
	req makeReq
	log *syncBuf
	lib string
}

func newMakeRig(t *testing.T, dur int64) *makeRig {
	t.Helper()
	lib, data := t.TempDir(), t.TempDir()
	p := filepath.Join(lib, "Film (2020)/Film (2020).mkv")
	touch(t, p)
	if err := os.WriteFile(p, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	av := newFilmAV(dur)
	log := &syncBuf{}
	m := &maker{dataDir: data, folders: []Folder{{"movies", lib}}, asr: av, queue: newASRQueue(), cache: newMadeCache(data),
		logf: func(f string, a ...any) { fmt.Fprintf(log, f+"\n", a...) },
		open: func(string) (video, io.Closer, error) { return av, io.NopCloser(nil), nil }}
	k := offerKey{Track: 1, Lang: "en", Size: fi.Size(), MTime: fi.ModTime().Unix()}
	return &makeRig{m: m, av: av, log: log, lib: lib,
		req: makeReq{Key: k.String(), Language: "en", FileID: 70, Folder: "movies", Path: "Film (2020)/Film (2020).mkv"}}
}

func (r *makeRig) run() ([]byte, error) { return r.m.make(context.Background(), r.req) }

// wantEveryLine checks the subtitle has every line the film says, once
// each, in order, at the time it is said.
func wantEveryLine(t *testing.T, b []byte, dur int64) {
	t.Helper()
	cues := parseSRTBack(t, b)
	checkCueLimits(t, cues)
	want := int((dur-speechLen)/speechEvery) + 1 // lines said whole before the end
	if len(cues) != want {
		t.Fatalf("%d lines, want %d", len(cues), want)
	}
	for i, c := range cues {
		if c.Text != fmt.Sprintf("Line %d is said here.", i) || c.StartMs != int64(i)*speechEvery {
			t.Fatalf("line %d: %q at %d", i, c.Text, c.StartMs)
		}
	}
}

func TestMakeChunkPlan(t *testing.T) {
	if chunkMs != 90_000 || chunkPadMs != 15_000 || uploadCapBytes != 64<<20 || minWindowMs != 30_000 || asrChunkTimeout != 10*time.Minute {
		t.Fatal("chunks must be 90 s with 15 s pads, 64 MiB, 30 s at least, 10 min per call")
	}
	for _, c := range []struct {
		dur    int64
		chunks int
	}{{60_000, 1}, {5 * 60_000, 4}, {2 * 3_600_000, 80}, {3 * 3_600_000, 120}, {90_001, 2}} {
		r := newMakeRig(t, c.dur)
		b, err := r.run()
		if err != nil {
			t.Fatalf("%d ms: %v", c.dur, err)
		}
		wantEveryLine(t, b, c.dur)
		if r.av.calls != c.chunks {
			t.Errorf("%d ms: %d uploads, want %d", c.dur, r.av.calls, c.chunks)
		}
		for i, rd := range r.av.reads {
			start := int64(i) * chunkMs
			if wantFrom := max(0, start-chunkPadMs); rd.from != wantFrom || rd.dur != start+chunkMs+chunkPadMs-wantFrom {
				t.Errorf("%d ms: chunk %d read %+v", c.dur, i, rd)
			}
		}
		for _, l := range r.av.langs {
			if l != "en" {
				t.Errorf("upload without its language: %q", l)
			}
		}
		left, _ := filepath.Glob(filepath.Join(r.m.dataDir, windowPattern))
		if len(left) != 0 {
			t.Errorf("audio files left: %v", left)
		}
	}
}

// A lossless track fills 64 MiB early: the next chunk starts where that
// one really ended, and nothing is lost or said twice.
func TestMakeCappedChunks(t *testing.T) {
	r := newMakeRig(t, 3_600_000)
	r.av.capMs = 100_000 // a window would be 120 s
	b, err := r.run()
	if err != nil {
		t.Fatal(err)
	}
	wantEveryLine(t, b, 3_600_000)
	if r.av.calls <= 3_600_000/chunkMs {
		t.Errorf("%d uploads for 1 h in pieces capped at 100 s", r.av.calls)
	}
	if got := r.av.reads[1].from; got != 100_000-chunkPadMs-chunkPadMs {
		t.Errorf("second chunk read from %d, want %d", got, 100_000-2*chunkPadMs)
	}
}

func TestMakeCappedTooShort(t *testing.T) {
	r := newMakeRig(t, 3_600_000)
	r.av.capMs = 40_000 // owns 25 s: under 30 s
	if _, err := r.run(); !errors.Is(err, errMakeRule) || !strings.Contains(err.Error(), "too large per second") {
		t.Fatalf("got %v", err)
	}
	if r.av.calls != 0 {
		t.Errorf("%d uploads", r.av.calls)
	}
}

// Refused before anything is read or sent.
func TestMakeRefusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(r *makeRig)
		want string
	}{
		{"not a key", func(r *makeRig) { r.req.Key = "track-2:en" }, "not one of this plugin's keys"},
		{"other language", func(r *makeRig) { r.req.Language = "fr" }, "offer was for en"},
		{"video changed", func(r *makeRig) {
			os.WriteFile(filepath.Join(r.lib, r.req.Path), []byte("an upgraded film, larger"), 0o644)
		}, "changed since the search"},
		{"no folder", func(r *makeRig) { r.req.Folder = "" }, "none of the plugin's folders"},
		{"unknown folder", func(r *makeRig) { r.req.Folder = "shows" }, "not approved"},
		{"escaping path", func(r *makeRig) { r.req.Path = "../etc/passwd" }, "refused path"},
		{"track gone", func(r *makeRig) { r.av.tracks[0].Number = 2 }, "no track 1"},
		{"commentary now", func(r *makeRig) { r.av.tracks[0].Commentary = true }, "not a dialogue"},
		{"other language now", func(r *makeRig) { r.av.tracks[0].Language = "ger" }, "not en"},
		{"no duration", func(r *makeRig) { r.av.dur = 0 }, "duration is unknown"},
	}
	for _, c := range cases {
		r := newMakeRig(t, 600_000)
		c.edit(r)
		_, err := r.run()
		if !errors.Is(err, errMakeRule) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
		if r.av.calls != 0 {
			t.Errorf("%s: %d uploads", c.name, r.av.calls)
		}
	}
}

// A second ask for the same key answers from the data folder.
func TestMakeCached(t *testing.T) {
	r := newMakeRig(t, 1_200_000)
	b1, err := r.run()
	if err != nil {
		t.Fatal(err)
	}
	calls := r.av.calls
	if got, ok := r.m.cached(r.req); !ok || string(got) != string(b1) {
		t.Fatal("not cached")
	}
	b2, err := r.run()
	if err != nil || string(b2) != string(b1) || r.av.calls != calls {
		t.Fatalf("second make: %v, %d more uploads", err, r.av.calls-calls)
	}
	// Once the video changes, the old subtitle is never handed out.
	p := filepath.Join(r.lib, r.req.Path)
	if err := os.WriteFile(p, []byte("an upgraded film, larger"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.m.cached(r.req); ok {
		t.Error("cached subtitle of the old video handed out")
	}
	if _, err := r.run(); !errors.Is(err, errMakeRule) {
		t.Errorf("make after the change: %v", err)
	}
	// Another file id with the same key is another make.
	other := r.req
	other.FileID = 71
	if _, ok := r.m.cached(other); ok {
		t.Error("cached across files")
	}
	if _, err := os.Stat(r.m.cache.workDir(madeID(r.req.FileID, r.req.Key))); !os.IsNotExist(err) {
		t.Errorf("work folder kept after the subtitle was saved: %v", err)
	}
}

// A stop after chunk 3 resumes at chunk 4, and the result is whole.
func TestMakeResumes(t *testing.T) {
	r := newMakeRig(t, 2*3_600_000)
	r.av.onCall = func(n int) error {
		if n == 4 {
			return fmt.Errorf("%w: server went away", ErrASR)
		}
		return nil
	}
	if _, err := r.run(); !errors.Is(err, ErrASR) || !strings.Contains(err.Error(), "chunk 4") {
		t.Fatalf("got %v", err)
	}
	r.av.onCall = nil
	r.av.reads = nil
	b, err := r.run()
	if err != nil {
		t.Fatal(err)
	}
	wantEveryLine(t, b, 2*3_600_000)
	if want := int(2*3_600_000/chunkMs) - 3; len(r.av.reads) != want || r.av.reads[0].from != 3*chunkMs-chunkPadMs {
		t.Errorf("resumed with %d reads from %+v, want %d from chunk 4", len(r.av.reads), r.av.reads[0], want)
	}
}

// Segments Whisper marks as not speech are left out and counted.
func TestMakeDropsNotSpeech(t *testing.T) {
	r := newMakeRig(t, 3*chunkMs)
	r.av.extra = func(w media.Window) []Segment {
		// 3 s past a line's start, after it ends and before the next.
		at := float64((3000-w.BaseMs%speechEvery+speechEvery)%speechEvery) / 1000
		return []Segment{
			{Start: at + 20, End: at + 20.5, Text: "Thanks for watching!", NoSpeechProb: f64(0.95)},
			{Start: at + 40, End: at + 40.5, Text: "[music]", NoSpeechProb: f64(0.7), AvgLogprob: f64(-1.4)},
		}
	}
	b, err := r.run()
	if err != nil {
		t.Fatal(err)
	}
	wantEveryLine(t, b, 3*chunkMs)
	if !strings.Contains(r.log.String(), "6 segments dropped as not speech") {
		t.Errorf("log: %s", r.log)
	}
}

// A film where Whisper hears nothing gets no subtitle.
func TestMakeNoSpeechError(t *testing.T) {
	r := newMakeRig(t, 3_600_000)
	r.m.asr = silentASR{}
	if _, err := r.run(); !errors.Is(err, errNoSpeech) || !strings.Contains(err.Error(), "in en") {
		t.Fatalf("got %v", err)
	}
}

// silentASR hears nothing.
type silentASR struct{}

func (s silentASR) TranscribeChunk(ctx context.Context, path, lang string) (Transcript, error) {
	return Transcript{Language: lang, Probability: 1, Duration: 600}, nil
}

// Speech across a cut is kept once: by the chunk it starts in.
func TestOwnSegmentsEcho(t *testing.T) {
	start := int64(600_000)
	base := start - chunkPadMs
	sec := func(ms int64) float64 { return float64(ms-base) / 1000 }
	tr := Transcript{Segments: []Segment{
		{Start: sec(start - 3000), End: sec(start + 1000), Text: "before the cut"},        // the previous chunk's
		{Start: sec(start + 1000), End: sec(start + 4000), Text: "rest of the last line"}, // heard again
		{Start: sec(start + 5000), End: sec(start + 7000), Text: "new line"},
		{Start: sec(start + 8000), End: sec(start + 9000), Text: "   "},
		{Start: sec(start + 700_000), End: sec(start + 701_000), Text: "next chunk's"},
	}}
	kept, dropped, _ := ownSegments(tr, base, start, start+chunkMs, start+4000)
	if len(kept) != 1 || kept[0].Text != "new line" || kept[0].StartMs != start+5000 || dropped != 0 {
		t.Fatalf("kept %+v, dropped %d", kept, dropped)
	}
	// Past the overlap, a line starting inside the last one is kept: only
	// the cut makes echoes.
	tr = Transcript{Segments: []Segment{{Start: sec(start + 60_000), End: sec(start + 61_000), Text: "late"}}}
	if kept, _, _ := ownSegments(tr, base, start, start+chunkMs, start+70_000); len(kept) != 1 {
		t.Errorf("kept %+v", kept)
	}
	// The first chunk has nothing before it: speech right at the start is
	// kept.
	tr = Transcript{Segments: []Segment{{Start: 0, End: 2.5, Text: "first"}, {Start: 4, End: 6.5, Text: "second"}}}
	if kept, _, _ := ownSegments(tr, 0, 0, chunkMs, -1); len(kept) != 2 {
		t.Errorf("start of the film: kept %+v", kept)
	}
}

// A lone thanks or "you" is dropped only where an upload or one of the
// engine's 30 s steps starts; the same words elsewhere, or with more words,
// are kept.
func TestOwnSegmentsEdgeFiller(t *testing.T) {
	if engineStepMs != 30_000 || edgeSlackMs != 250 {
		t.Fatal("edges are every 30 s, with 250 ms either side")
	}
	for _, c := range []struct {
		text string
		at   float64 // seconds into the upload
		drop bool
	}{
		{"you", 15.0, false},  // inside a step: said by someone
		{" you", 30.0, true},  // a 30 s mark
		{"You.", 0.0, true},   // the upload's start
		{"you", 60.25, true},  // 250 ms after a mark
		{"you", 59.75, true},  // 250 ms before one
		{"you", 60.26, false}, // just past
		{"you", 89.74, false}, // just before
		{"Thank you.", 90.1, true},
		{"Thank", 30.0, true},
		{"Thank you for watching!", 60.0, true},
		{"Thanks for watching.", 60.0, true},
		{"thank-you", 30.0, true},
		{"Thank you, Darrell.", 30.0, false}, // more than the filler
		{"You're", 30.0, false},
		{"Thanks.", 30.0, false}, // not in the list
		{"Thank you.", 45.0, false},
	} {
		tr := Transcript{Segments: []Segment{{Start: c.at, End: c.at + 1, Text: c.text}}}
		// The upload starts at 75 s and owns from 90 s; the times above are
		// past its own start.
		kept, _, fillers := ownSegments(tr, 75_000-15_000, 75_000, 75_000+chunkMs*2, -1)
		if c.at*1000+60_000 < 75_000 {
			kept, _, fillers = ownSegments(tr, 0, 0, chunkMs*2, -1)
		}
		if got := len(kept) == 0; got != c.drop || len(fillers) != map[bool]int{true: 1}[c.drop] {
			t.Errorf("%q at %.2f s: kept %v, fillers %v, want dropped %v", c.text, c.at, kept, fillers, c.drop)
		}
	}
}

// Fillers at edges are logged one by one and counted per make.
func TestMakeLogsEdgeFillers(t *testing.T) {
	r := newMakeRig(t, 3*chunkMs)
	r.av.extra = func(w media.Window) []Segment {
		if w.BaseMs != 0 {
			return nil
		}
		// The first upload: 30 s is a mark, 45 s is not.
		return []Segment{{Start: 30, End: 30.5, Text: " you"}, {Start: 45, End: 45.5, Text: " you"}}
	}
	b, err := r.run()
	if err != nil {
		t.Fatal(err)
	}
	cues := parseSRTBack(t, b)
	yous := 0
	for _, c := range cues {
		if c.Text == "you" {
			yous++
		}
	}
	if yous != 1 {
		t.Errorf("%d lone yous kept, want the one at 45 s", yous)
	}
	if !strings.Contains(r.log.String(), `dropped "you" at 00:00:30,000, 30.0 s into the upload`) ||
		!strings.Contains(r.log.String(), "0 segments dropped as not speech, 1 as filler at a piece edge") {
		t.Errorf("log: %s", r.log)
	}
}

func TestOfferKey(t *testing.T) {
	k := offerKey{Track: 12, Lang: "en", Size: 54_321_987_654, MTime: 1_764_150_000}
	s := k.String()
	if s != "v1:t12:en:54321987654:1764150000" || len(s) > 200 {
		t.Errorf("key %q", s)
	}
	back, err := parseKey(s)
	if err != nil || back != k {
		t.Errorf("parsed %+v, %v", back, err)
	}
	for _, bad := range []string{"", "track-2:en", "v1:t2:EN:1:2", "v1:t2:en:1", "v1:t2:en:1:2:3", "v1:t2:en:-1:2", "v2:t2:en:1:2"} {
		if _, err := parseKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCachePrune(t *testing.T) {
	dir := t.TempDir()
	c := newMadeCache(dir)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	put := func(i int, age time.Duration) string {
		at := now.Add(-age)
		c.now = func() time.Time { return at }
		id := madeID(int64(i), "k")
		if err := c.put(id, int64(i), "k", []byte("1\n00:00:01,000 --> 00:00:02,000\nx\n")); err != nil {
			t.Fatal(err)
		}
		c.now = func() time.Time { return now }
		return id
	}
	old := put(1, 7*24*time.Hour+time.Minute)
	fresh := put(2, 7*24*time.Hour-time.Minute)
	for i := range madeMax {
		put(100+i, time.Duration(i)*time.Second)
	}
	// An unfinished make: one fresh, one left 3 days ago.
	if err := c.openWork("w1", 5, "k"); err != nil {
		t.Fatal(err)
	}
	if err := c.openWork("w2", 6, "k"); err != nil {
		t.Fatal(err)
	}
	three := now.Add(-3 * 24 * time.Hour)
	os.Chtimes(c.workDir("w2"), three, three)
	os.Chtimes(c.workDir("w1"), now, now)
	if err := c.prune(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.get(old, 1, "k"); ok {
		t.Error("over 7 days kept")
	}
	if _, ok := c.get(fresh, 2, "k"); ok {
		t.Error("201st newest kept")
	}
	if _, ok := c.get(madeID(100, "k"), 100, "k"); !ok {
		t.Error("newest dropped")
	}
	ents, _ := os.ReadDir(c.madeDir())
	if len(ents) != 2*madeMax {
		t.Errorf("%d files kept, want %d", len(ents), 2*madeMax)
	}
	if _, err := os.Stat(c.workDir("w1")); err != nil {
		t.Error("fresh work folder removed")
	}
	if _, err := os.Stat(c.workDir("w2")); !os.IsNotExist(err) {
		t.Error("3 day old work folder kept")
	}
	if madeKeep != 7*24*time.Hour || madeMax != 200 || workKeep != 2*24*time.Hour {
		t.Error("keep 7 days and 200 subtitles, work folders 2 days")
	}
}
