package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"whisper-subtitles/media"
)

// writeSRT writes cues as an SRT file.
func writeSRT(t *testing.T, p string, cues []Cue) {
	t.Helper()
	ts := func(ms int64) string {
		return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
	}
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, ts(c.StartMs), ts(c.EndMs), c.Text)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeAV is a video and a Whisper server at once: Extract records the
// window, and Transcribe answers with what hearWords made for it.
type fakeAV struct {
	dur     int64
	tracks  []media.Track
	heard   map[int64]Transcript // by window start
	mu      sync.Mutex
	last    int64
	extract []struct{ start, dur, max int64 }
	langs   []string
	paths   []string
	names   []string          // uploadName of each upload
	onCall  func(n int) error // optional: an error for upload n (from 1)
	calls   int
}

func (f *fakeAV) Duration() int64          { return f.dur }
func (f *fakeAV) AllTracks() []media.Track { return f.tracks }
func (f *fakeAV) Extract(t media.Track, start, dur, max int64, w io.Writer) (media.Window, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = start
	f.extract = append(f.extract, struct{ start, dur, max int64 }{start, dur, max})
	n, err := w.Write(bytes.Repeat([]byte("a"), 1000))
	return media.Window{BaseMs: start, EndMs: start + dur, Blocks: 10, Bytes: int64(n)}, err
}

func (f *fakeAV) Transcribe(ctx context.Context, path, lang string) (Transcript, error) {
	f.mu.Lock()
	f.calls++
	n, start := f.calls, f.last
	f.langs = append(f.langs, lang)
	f.paths = append(f.paths, path)
	f.names = append(f.names, uploadName(ctx))
	hook := f.onCall
	f.mu.Unlock()
	if _, err := os.Stat(path); err != nil {
		return Transcript{}, fmt.Errorf("window file missing during upload: %v", err)
	}
	if hook != nil {
		if err := hook(n); err != nil {
			return Transcript{}, err
		}
	}
	return f.heard[start], nil
}

func engTrack() media.Track {
	return media.Track{Number: 1, Audio: true, Enabled: true, Default: true, CodecID: "A_AAC", Language: "eng"}
}

// newFakeAV hears the film's cues placed at k*cue + b, with word timings.
func newFakeAV(cues []Cue, k, b float64) *fakeAV {
	f := &fakeAV{dur: filmMs, tracks: []media.Track{engTrack()}, heard: map[int64]Transcript{}}
	for _, w := range hearWords(cues, filmMs, k, b, "en", 31) {
		f.heard[w.BaseMs] = w.Tr
	}
	return f
}

// fakeTitle serves title 7's subtitles (subtitle 700 on file 70) and
// records sync calls. row can change between calls.
type fakeTitle struct {
	mu    sync.Mutex
	row   SubRow
	gets  atomic.Int32
	syncs []string
}

func (ft *fakeTitle) server(t *testing.T) *HolyArrs {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"no token"}`, http.StatusUnauthorized)
			return
		}
		ft.mu.Lock()
		defer ft.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/titles/7/subtitles":
			ft.gets.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"files": []FileView{{FileID: 70, RelativePath: "Film/Film.mkv", Subtitles: []SubRow{ft.row}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/subtitles/700/sync":
			b, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("sync sent as %q", r.Header.Get("Content-Type"))
			}
			ft.syncs = append(ft.syncs, string(b))
			nr := ft.row
			nr.ID = 701
			json.NewEncoder(w).Encode(nr)
		default:
			http.Error(w, `{"error":"no such route"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	ha, err := newHolyArrs(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return ha
}

type checkRig struct {
	k    *checker
	av   *fakeAV
	ft   *fakeTitle
	c    Candidate
	loc  Located
	log  *syncBuf
	data string
	cues []Cue
}

func newRig(t *testing.T, av *fakeAV, cues []Cue) *checkRig {
	t.Helper()
	lib, data := t.TempDir(), t.TempDir()
	writeSRT(t, filepath.Join(lib, "Film/Film.en.srt"), cues)
	touch(t, filepath.Join(lib, "Film/Film.mkv"))
	loc, err := locate([]Folder{{"movies", lib}}, "Film/Film.mkv", "Film/Film.en.srt")
	if err != nil {
		t.Fatal(err)
	}
	rel, score := "Film/Film.en.srt", 80
	ft := &fakeTitle{row: SubRow{ID: 700, FileID: 70, Language: "en", Place: "beside", RelativePath: &rel,
		Score: &score, Origin: "fetched", State: "present"}}
	log := &syncBuf{}
	k := &checker{ha: ft.server(t), asr: av, queue: newASRQueue(), dataDir: data, logf: func(f string, a ...any) { fmt.Fprintf(log, f+"\n", a...) },
		open: func(string) (video, io.Closer, error) { return av, io.NopCloser(nil), nil }}
	c := Candidate{TitleID: 7, SubtitleID: 700, FileID: 70, VideoRel: "Film/Film.mkv", SubRel: rel, Language: "en", Format: "srt", Score: &score}
	return &checkRig{k: k, av: av, ft: ft, c: c, loc: loc, log: log, data: data, cues: cues}
}

func (r *checkRig) run() error { return r.k.check(context.Background(), r.c, r.loc) }

func (r *checkRig) syncBody(t *testing.T) SyncFix {
	t.Helper()
	r.ft.mu.Lock()
	defer r.ft.mu.Unlock()
	if len(r.ft.syncs) != 1 {
		t.Fatalf("%d sync calls, want 1: %v", len(r.ft.syncs), r.ft.syncs)
	}
	var f SyncFix
	dec := json.NewDecoder(strings.NewReader(r.ft.syncs[0]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

func (r *checkRig) noSync(t *testing.T) {
	t.Helper()
	r.ft.mu.Lock()
	defer r.ft.mu.Unlock()
	if len(r.ft.syncs) != 0 {
		t.Errorf("sync sent: %v", r.ft.syncs)
	}
}

// noWindowFiles: every window file was removed after its upload.
func (r *checkRig) noWindowFiles(t *testing.T) {
	t.Helper()
	if m, _ := filepath.Glob(filepath.Join(r.data, "*")); len(m) != 0 {
		t.Errorf("left in the data folder: %v", m)
	}
}

func TestCheckSyncsOffset(t *testing.T) {
	cues := film(21, filmMs)
	r := newRig(t, newFakeAV(cues, 1, 2500), cues)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	f := r.syncBody(t)
	if math.Abs(float64(f.OffsetMS)-2500) > 30 || f.FromFPS != 0 || f.ToFPS != 0 {
		t.Errorf("sent %+v, want offset 2500 within 30 and no frame rates", f)
	}
	if !strings.Contains(r.ft.syncs[0], `"offset_ms"`) || strings.Contains(r.ft.syncs[0], "fps") {
		t.Errorf("body %s", r.ft.syncs[0])
	}
	// 12 windows of 90 s, capped at 64 MiB, each sent with the track's
	// language, from files in the data folder that are gone afterwards.
	starts := media.WindowStarts(filmMs)
	if len(r.av.extract) != len(starts) {
		t.Fatalf("%d windows, want %d", len(r.av.extract), len(starts))
	}
	for i, e := range r.av.extract {
		if e.start != starts[i] || e.dur != media.WindowMs || e.max != uploadCapBytes {
			t.Errorf("window %d: %+v", i, e)
		}
	}
	for i, l := range r.av.langs {
		if l != "en" {
			t.Errorf("upload %d sent language %q, want en", i, l)
		}
		if filepath.Dir(r.av.paths[i]) != r.data || !strings.HasPrefix(filepath.Base(r.av.paths[i]), "window-") {
			t.Errorf("upload %d from %s", i, r.av.paths[i])
		}
	}
	r.noWindowFiles(t)
	if !strings.Contains(r.log.String(), "synced") || !strings.Contains(r.log.String(), "now subtitle 701") {
		t.Errorf("log: %s", r.log)
	}
	// Re-read before the sync: one GET.
	if r.ft.gets.Load() != 1 {
		t.Errorf("%d reads of the title, want 1", r.ft.gets.Load())
	}
}

func TestCheckSyncsFrameRate(t *testing.T) {
	cues := film(22, filmMs)
	r := newRig(t, newFakeAV(cues, 25/(24000.0/1001), -120), cues)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	f := r.syncBody(t)
	if f.FromFPS != 25 || f.ToFPS != 23.976 || math.Abs(float64(f.OffsetMS)+120) > 30 {
		t.Errorf("sent %+v, want 25 to 23.976 and -120 ms", f)
	}
}

func TestCheckOnlySendsNothing(t *testing.T) {
	cues := film(23, filmMs)
	r := newRig(t, newFakeAV(cues, 1, 2500), cues)
	r.k.checkOnly = true
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	r.noSync(t)
	if !strings.Contains(r.log.String(), `check only, would send {"offset_ms":25`) {
		t.Errorf("log: %s", r.log)
	}
}

func TestCheckInSyncLeftAlone(t *testing.T) {
	cues := film(24, filmMs)
	r := newRig(t, newFakeAV(cues, 1, 0), cues)
	err := r.run()
	if !errors.Is(err, errRule) || !strings.Contains(err.Error(), "already in sync") {
		t.Errorf("got %v", err)
	}
	r.noSync(t)
	r.noWindowFiles(t)
	if r.ft.gets.Load() != 0 {
		t.Errorf("HolyArrs read %d times for a subtitle left alone", r.ft.gets.Load())
	}
}

// The file or the row changing while the audio was checked stops the sync.
func TestCheckChangedBeforeSync(t *testing.T) {
	cues := film(25, filmMs)
	for _, c := range []struct {
		name   string
		change func(r *checkRig)
		want   string
	}{
		{"file edited", func(r *checkRig) {
			os.WriteFile(r.loc.Sub, []byte("1\n00:00:01,000 --> 00:00:02,000\nhello\n"), 0o644)
		}, "the file changed"},
		{"synced by hand", func(r *checkRig) {
			r.ft.mu.Lock()
			r.ft.row.Sync = map[string]any{"offset_ms": 500}
			r.ft.mu.Unlock()
		}, "timing was changed"},
		{"replaced", func(r *checkRig) {
			r.ft.mu.Lock()
			r.ft.row.State = "replaced"
			r.ft.mu.Unlock()
		}, "changed in HolyArrs"},
		{"removed", func(r *checkRig) {
			r.ft.mu.Lock()
			r.ft.row.ID = 999
			r.ft.mu.Unlock()
		}, "gone"},
	} {
		r := newRig(t, newFakeAV(cues, 1, 2500), cues)
		r.av.onCall = func(n int) error {
			if n == media.Windows {
				c.change(r)
			}
			return nil
		}
		err := r.run()
		if !errors.Is(err, errRule) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
		r.noSync(t)
	}
}

func TestCheckUntaggedTrackSendsNoLanguage(t *testing.T) {
	cues := film(26, filmMs)
	av := newFakeAV(cues, 1, 2500)
	tr := engTrack()
	tr.Language = "und"
	av.tracks = []media.Track{tr}
	r := newRig(t, av, cues)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	for i, l := range av.langs {
		if l != "" {
			t.Errorf("upload %d sent language %q for an untagged track", i, l)
		}
	}
	r.syncBody(t)
}

func TestCheckOtherLanguageUploadsNothing(t *testing.T) {
	cues := film(27, filmMs)
	av := newFakeAV(cues, 1, 2500)
	tr := engTrack()
	tr.Language = "spa"
	av.tracks = []media.Track{tr}
	r := newRig(t, av, cues)
	err := r.run()
	if !errors.Is(err, errRule) || !strings.Contains(err.Error(), "spa") {
		t.Errorf("got %v", err)
	}
	if len(av.extract) != 0 || av.calls != 0 {
		t.Errorf("%d windows cut, %d uploads for a Spanish-only video", len(av.extract), av.calls)
	}
}

// A track's bitrate shortens the window to fit 64 MiB, down to 30 s.
func TestCheckWindowLengthByBitrate(t *testing.T) {
	cues := film(28, filmMs)
	for _, c := range []struct {
		bps    int64
		durMs  int64
		refuse bool
	}{
		{640_000, media.WindowMs, false},                            // AC-3 5.1: 90 s is 7.2 MB
		{8_000_000, uploadCapBytes * 8 * 1000 / 8_000_000, false},   // 67.1 s
		{17_800_000, uploadCapBytes * 8 * 1000 / 17_800_000, false}, // just over 30 s
		{18_000_000, 0, true},                                       // 29.8 s: refused
	} {
		av := newFakeAV(cues, 1, 2500)
		tr := engTrack()
		tr.BPS = c.bps
		av.tracks = []media.Track{tr}
		r := newRig(t, av, cues)
		err := r.run()
		if c.refuse {
			if !errors.Is(err, errRule) || len(av.extract) != 0 {
				t.Errorf("%d b/s: %v, %d windows cut; want refused before cutting", c.bps, err, len(av.extract))
			}
			continue
		}
		if len(av.extract) == 0 || av.extract[0].dur != c.durMs {
			t.Errorf("%d b/s: windows %+v, want %d ms", c.bps, av.extract, c.durMs)
		}
	}
}

// A sync window behind a make's upload logs that it waits and when it gets
// the slot; a window that finds it free logs nothing about it.
func TestCheckLogsSlotWait(t *testing.T) {
	cues := film(31, filmMs)
	r := newRig(t, newFakeAV(cues, 1, 2500), cues)
	hold, err := r.k.queue.acquire(context.Background(), prioMake)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- r.run() }()
	waitQueued(t, r.k.queue, prioSync, 1)
	time.Sleep(200 * time.Millisecond)
	hold()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	log := r.log.String()
	n := len(media.WindowStarts(filmMs))
	wait := fmt.Sprintf("subtitle 700 (en, Film/Film.en.srt), window 1 of %d: waiting for the Whisper slot, held by a subtitle being written (1 upload goes first)\n", n)
	if !strings.Contains(log, wait) || !regexp.MustCompile(fmt.Sprintf(`window 1 of %d: got the Whisper slot after ([2-9]00ms|[0-9.]+s)\n`, n)).MatchString(log) {
		t.Errorf("log: %s", log)
	}
	if strings.Count(log, "Whisper slot") != 2 {
		t.Errorf("windows that found the slot free logged it: %s", log)
	}
}

// An upload with no answer in time ends the check at once, and the next
// upload waits for the pause, which doubles up to its limit and ends with
// an answer.
func TestCheckUploadTimeoutPauses(t *testing.T) {
	cues := film(29, filmMs)
	r := newRig(t, newFakeAV(cues, 1, 2500), cues)
	r.k.queue = newASRQueueWith(150*time.Millisecond, 400*time.Millisecond)
	r.av.onCall = func(n int) error {
		if n == 3 {
			return fmt.Errorf("%w (10m0s): context deadline exceeded", errASRTimeout)
		}
		return nil
	}
	err := r.run()
	if !errors.Is(err, errASRTimeout) || r.av.calls != 3 {
		t.Fatalf("got %v after %d uploads; want the timeout after 3", err, r.av.calls)
	}
	r.noSync(t)
	r.noWindowFiles(t)
	if !strings.Contains(r.log.String(), "no uploads for 150ms") {
		t.Errorf("log: %s", r.log)
	}
	// The next check waits out the pause before its first upload.
	r.av.onCall = nil
	t0 := time.Now()
	var first time.Duration
	r.av.onCall = func(n int) error {
		if n == 4 {
			first = time.Since(t0)
		}
		return nil
	}
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if first < 100*time.Millisecond {
		t.Errorf("first upload after %s, inside the 150 ms pause", first)
	}
	// Doubling, capped, and reset by an answer.
	g := newASRQueueWith(time.Second, 3*time.Second)
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second} {
		if got := g.timedOut(); got != want {
			t.Errorf("timeout %d: pause %s, want %s", i+1, got, want)
		}
	}
	g.answered()
	if got := g.timedOut(); got != time.Second {
		t.Errorf("after an answer: pause %s, want %s", got, time.Second)
	}
	// A stop while paused ends the wait.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.acquire(ctx, prioSync); !errors.Is(err, context.Canceled) {
		t.Errorf("wait after a stop: %v", err)
	}
	if backoffFirst != 5*time.Minute || backoffMax != time.Hour || asrUploadTimeout != 10*time.Minute {
		t.Errorf("pause %s to %s, upload limit %s; want 5m, 1h and 10m", backoffFirst, backoffMax, asrUploadTimeout)
	}
}

// The real client: a server that answers too late is a timeout, tried once.
func TestTranscribeTimeoutNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	w := newWhisper(srv.URL)
	w.RetryWait = time.Millisecond
	w.HTTP.Timeout = 50 * time.Millisecond
	_, err := w.Transcribe(context.Background(), windowFile(t, []byte("mka")), "en")
	if !errors.Is(err, errASRTimeout) || !errors.Is(err, ErrASR) {
		t.Errorf("got %v, want a timeout", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d calls, want 1: a timeout is not retried", n)
	}
}

func TestOpenVideoRefusesOtherContainers(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"film.mp4", "film.avi", "film.ts"} {
		p := filepath.Join(dir, name)
		touch(t, p)
		if _, _, err := openVideo(p); !errors.Is(err, errRule) || !strings.Contains(err.Error(), "not supported yet") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A .mkv that isn't one is left alone, not a crash.
	p := filepath.Join(dir, "broken.mkv")
	os.WriteFile(p, []byte("not matroska"), 0o644)
	if _, _, err := openVideo(p); !errors.Is(err, errRule) {
		t.Errorf("broken.mkv: %v", err)
	}
}

// The app uses the real check once the Whisper address is set.
func TestAppUsesChecker(t *testing.T) {
	a, _ := testApp(t, readySettings(), t.TempDir())
	if a.check == nil {
		t.Fatal("no check")
	}
	// A candidate whose video is an MP4 is left alone by the real check.
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "f.mp4"))
	writeSRT(t, filepath.Join(dir, "f.en.srt"), film(30, 60_000))
	err := a.check(context.Background(), Candidate{Language: "en", Format: "srt"}, Located{Video: filepath.Join(dir, "f.mp4"), Sub: filepath.Join(dir, "f.en.srt")})
	if !errors.Is(err, errRule) || !strings.Contains(err.Error(), "not supported yet") {
		t.Errorf("got %v", err)
	}
}

// A sync keeps going through a Whisper outage and syncs once it is back.
func TestCheckWaitsForWhisper(t *testing.T) {
	cues := film(33, filmMs)
	r := newRig(t, newFakeAV(cues, 1, 2500), cues)
	d := &downServer{}
	d.up.Store(true)
	r.k.queue.watchDown(d.probe, r.k.logf)
	r.k.queue.probeEvery = 20 * time.Millisecond
	r.av.onCall = func(n int) error {
		if n == 4 {
			d.up.Store(false)
			go func() { time.Sleep(150 * time.Millisecond); d.up.Store(true) }()
		}
		if !d.up.Load() {
			return fmt.Errorf("%w (whisper.test:9000 answered 502)", errASRDown)
		}
		return nil
	}
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	if f := r.syncBody(t); math.Abs(float64(f.OffsetMS)-2500) > 30 {
		t.Errorf("sent %+v", f)
	}
	if l := r.log.String(); strings.Count(l, "went away") != 1 || strings.Count(l, "is back after") != 1 {
		t.Errorf("log: %s", l)
	}
}

// A sync whose upload gets a 504 while GET /asr answers ends as a timeout
// and starts the backoff, like an upload with no answer.
func TestCheckGatewayBusyBacksOff(t *testing.T) {
	cues := film(35, filmMs)
	r := newRig(t, newFakeAV(cues, 1, 2500), cues)
	d := &downServer{}
	d.up.Store(true)
	r.k.queue.watchDown(d.probe, r.k.logf)
	r.av.onCall = func(n int) error {
		if n == 3 {
			return fmt.Errorf("%w 504 for http://whisper.test:9000: Gateway Timeout", errASRGateway)
		}
		return nil
	}
	err := r.run()
	if !errors.Is(err, errASRTimeout) || r.av.calls != 3 {
		t.Fatalf("got %v after %d uploads; want a timeout after 3", err, r.av.calls)
	}
	r.noSync(t)
	if l := r.log.String(); !strings.Contains(l, "no uploads for 5m0s") || strings.Contains(l, "went away") {
		t.Errorf("log: %s", l)
	}
}

// Each sync upload is named by its window, for the Whisper client's log.
func TestCheckNamesUploads(t *testing.T) {
	cues := film(37, filmMs)
	r := newRig(t, newFakeAV(cues, 1, 2500), cues)
	if err := r.run(); err != nil {
		t.Fatal(err)
	}
	n := len(media.WindowStarts(filmMs))
	if len(r.av.names) != n || r.av.names[2] != fmt.Sprintf("subtitle 700 (en, Film/Film.en.srt), window 3 of %d", n) {
		t.Errorf("names %q", r.av.names)
	}
}
