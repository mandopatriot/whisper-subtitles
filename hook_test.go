package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hookAnswer is one answer the fake hook server got.
type hookAnswer struct {
	Job   string          `json:"job"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

// fakeHook is HolyArrs's side of the subtitle site hook: it hands out the
// jobs a test pushes, answers 204 after a short wait when there are none,
// and records answers.
type fakeHook struct {
	t       *testing.T
	jobs    chan hookJob
	answers chan hookAnswer
	gets    atomic.Int32
	n       atomic.Int32
	// getStatus, when set, gives a status to answer poll n with instead.
	getStatus atomic.Value // func(n int32) int
	// answerStatus, when set, gives the status for an answer.
	answerStatus atomic.Value // func(job string) int
	srv          *httptest.Server
}

func newFakeHook(t *testing.T) *fakeHook {
	h := &fakeHook{t: t, jobs: make(chan hookJob, 64), answers: make(chan hookAnswer, 64)}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHook) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, `{"error":"no token"}`, http.StatusUnauthorized)
		return
	}
	if r.URL.Path != subtitleHookPath {
		http.Error(w, `{"error":"no such route"}`, http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		n := h.gets.Add(1)
		if f, ok := h.getStatus.Load().(func(int32) int); ok && f != nil {
			if code := f(n); code != 0 {
				http.Error(w, `{"error":"refused by the test"}`, code)
				return
			}
		}
		select {
		case j := <-h.jobs:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(j)
		case <-time.After(20 * time.Millisecond):
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	case http.MethodPost:
		var a hookAnswer
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil || a.Job == "" {
			h.t.Errorf("bad answer: %v", err)
			http.Error(w, `{"error":"bad"}`, http.StatusBadRequest)
			return
		}
		if (a.Error == "") == (len(a.Data) == 0) {
			h.t.Errorf("answer %s has both or neither of data and error", a.Job)
		}
		if f, ok := h.answerStatus.Load().(func(string) int); ok && f != nil {
			if code := f(a.Job); code != 0 {
				http.Error(w, `{"error":"no such job waiting"}`, code)
				return
			}
		}
		h.answers <- a
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// push hands the plugin a job and returns its id.
func (h *fakeHook) push(op string, payload any) string {
	b, _ := json.Marshal(payload)
	id := fmt.Sprintf("job-%d", h.n.Add(1))
	h.jobs <- hookJob{ID: id, Op: op, Payload: b}
	return id
}

// answer waits for the answer to job.
func (h *fakeHook) answer(job string) hookAnswer {
	h.t.Helper()
	for {
		select {
		case a := <-h.answers:
			if a.Job == job {
				return a
			}
			h.answers <- a // someone else's; put it back
			time.Sleep(time.Millisecond)
		case <-time.After(10 * time.Second):
			h.t.Fatalf("no answer to %s", job)
		}
	}
}

func (h *fakeHook) noAnswer(wait time.Duration) {
	h.t.Helper()
	select {
	case a := <-h.answers:
		h.t.Errorf("unexpected answer %+v", a)
	case <-time.After(wait):
	}
}

type siteRig struct {
	*makeRig
	hook   *fakeHook
	srv    *siteServer
	cancel context.CancelFunc
	done   chan struct{}
}

// newSiteRig runs a subtitle site against a fake hook server, writing from
// a fake film of dur ms.
func newSiteRig(t *testing.T, dur int64) *siteRig {
	t.Helper()
	mr := newMakeRig(t, dur)
	hook := newFakeHook(t)
	ha, err := newHolyArrs(hook.srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	set := defaults()
	set.ASRURL = "http://whisper.test:9000"
	s := &searcher{set: set, folders: mr.m.folders, wait: searchWait, logf: mr.m.logf,
		tracks: newTrackCache(func(string) (video, io.Closer, error) { return mr.av, nopCloser{}, nil })}
	srv := newSiteServer(ha, s, mr.m, mr.m.logf, time.Hour)
	srv.authWait, srv.firstBackoff, srv.maxBackoff, srv.answerWait = 100*time.Millisecond, 20*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	r := &siteRig{makeRig: mr, hook: hook, srv: srv, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(r.done); srv.run(ctx) }()
	t.Cleanup(r.stop)
	return r
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func (r *siteRig) stop() {
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		panic("the subtitle site did not stop")
	}
}

func (r *siteRig) ask() searchAsk {
	return searchAsk{Kind: "movie", Title: "Film", Languages: []string{"en", "fr"}, FileID: r.req.FileID, Folder: r.req.Folder, Path: r.req.Path}
}

// block makes uploads wait until the returned func is called.
func (r *siteRig) block() (release func()) {
	gate := make(chan struct{})
	r.av.mu.Lock()
	r.av.onCall = func(int) error { <-gate; return nil }
	r.av.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func readMade(t *testing.T, a hookAnswer) []byte {
	t.Helper()
	if a.Error != "" {
		t.Fatalf("make answered error %q", a.Error)
	}
	var m madeAnswer
	if err := json.Unmarshal(a.Data, &m); err != nil || m.Format != "srt" {
		t.Fatalf("made answer %s: %v", a.Data, err)
	}
	return m.Body
}

// Search then make, over HTTP with the plugin's token, as HolyArrs does it.
func TestSiteSearchAndMake(t *testing.T) {
	r := newSiteRig(t, 300_000)
	a := r.hook.answer(r.hook.push("search", r.ask()))
	var offers []offer
	if err := json.Unmarshal(a.Data, &offers); err != nil || len(offers) != 1 {
		t.Fatalf("offers %s: %v", a.Data, err)
	}
	if offers[0].Key != r.req.Key || offers[0].Language != "en" {
		t.Fatalf("offer %+v, want %s", offers[0], r.req.Key)
	}
	if r.av.calls != 0 {
		t.Fatal("search sent audio to Whisper")
	}
	body := readMade(t, r.hook.answer(r.hook.push("make", r.req)))
	wantEveryLine(t, body, 300_000)
	// A search with nothing to offer answers an empty list, not null.
	ask := r.ask()
	ask.Languages = []string{"de"}
	if a := r.hook.answer(r.hook.push("search", ask)); string(a.Data) != "[]" {
		t.Errorf("no offer answered %s", a.Data)
	}
}

// Polling goes on while a make runs, so HolyArrs keeps the site live.
func TestSiteKeepsPollingDuringMake(t *testing.T) {
	r := newSiteRig(t, 300_000)
	release := r.block()
	job := r.hook.push("make", r.req)
	waitFor(t, "the upload to start", func() bool { r.av.mu.Lock(); defer r.av.mu.Unlock(); return r.av.calls == 1 })
	before := r.hook.gets.Load()
	waitFor(t, "more polls during the make", func() bool { return r.hook.gets.Load() >= before+5 })
	// A search during the make is answered at once.
	if a := r.hook.answer(r.hook.push("search", r.ask())); len(a.Data) < 3 {
		t.Errorf("search during a make: %s", a.Data)
	}
	release()
	wantEveryLine(t, readMade(t, r.hook.answer(job)), 300_000)
}

// 401 and 403 are logged once and retried slowly; other errors back off.
func TestSitePollRefused(t *testing.T) {
	r := newSiteRig(t, 300_000)
	var refuse atomic.Bool
	refuse.Store(true)
	r.hook.getStatus.Store(func(n int32) int {
		if refuse.Load() {
			return http.StatusForbidden
		}
		return 0
	})
	time.Sleep(450 * time.Millisecond)
	if n := r.hook.gets.Load(); n > 7 {
		t.Errorf("%d polls in 450 ms while refused, want a 100 ms wait between", n)
	}
	if c := strings.Count(r.log.String(), "refused the subtitle site hook"); c != 1 {
		t.Errorf("refusal logged %d times:\n%s", c, r.log)
	}
	refuse.Store(false)
	waitFor(t, "polling to resume", func() bool { return strings.Contains(r.log.String(), "accepts the subtitle site hook again") })
	// A server error backs off, doubling up to the cap.
	var fails atomic.Int32
	r.hook.getStatus.Store(func(n int32) int {
		if fails.Add(1) <= 3 {
			return http.StatusBadGateway
		}
		return 0
	})
	waitFor(t, "polling after server errors", func() bool { return fails.Load() > 4 })
	if l := r.log.String(); !strings.Contains(l, "trying again in 20ms") || !strings.Contains(l, "trying again in 40ms") || !strings.Contains(l, "trying again in 50ms") {
		t.Errorf("log: %s", r.log)
	}
}

// HolyArrs no longer waits (it gave up or restarted): logged, and the file
// stays for the next ask, which is answered at once.
func TestSiteAnswerGone(t *testing.T) {
	r := newSiteRig(t, 300_000)
	r.hook.answerStatus.Store(func(job string) int {
		if job == "job-1" {
			return http.StatusNotFound
		}
		return 0
	})
	r.hook.push("make", r.req)
	waitFor(t, "the lost answer", func() bool { return strings.Contains(r.log.String(), "no longer waits") })
	if _, ok := r.m.cached(r.req); !ok {
		t.Fatal("finished subtitle not kept")
	}
	calls := r.av.calls
	wantEveryLine(t, readMade(t, r.hook.answer(r.hook.push("make", r.req))), 300_000)
	if r.av.calls != calls {
		t.Error("the second ask sent audio again")
	}
}

// Two asks for one subtitle share one make; past 20 queued, "busy".
func TestSiteJoinsAndBusy(t *testing.T) {
	r := newSiteRig(t, 300_000)
	release := r.block()
	defer release()
	first := r.hook.push("make", r.req)
	waitFor(t, "the first make to start", func() bool { r.av.mu.Lock(); defer r.av.mu.Unlock(); return r.av.calls == 1 })
	second := r.hook.push("make", r.req)
	waitFor(t, "the join", func() bool { return strings.Contains(r.log.String(), "already being written") })
	var ids []string
	for i := range maxQueuedMakes - 1 {
		req := r.req
		req.FileID = int64(1000 + i)
		ids = append(ids, r.hook.push("make", req))
	}
	over := r.req
	over.FileID = 5000
	a := r.hook.answer(r.hook.push("make", over))
	if a.Error != "busy, 20 subtitles already queued" {
		t.Errorf("21st make: %+v", a)
	}
	release()
	b1, b2 := readMade(t, r.hook.answer(first)), readMade(t, r.hook.answer(second))
	if string(b1) != string(b2) {
		t.Error("joined asks got different files")
	}
	for _, id := range ids {
		r.hook.answer(id)
	}
	if maxQueuedMakes != 20 {
		t.Error("at most 20 makes queued")
	}
}

// A stop mid-make keeps the finished chunks and sends no answer.
func TestSiteStopKeepsChunks(t *testing.T) {
	r := newSiteRig(t, 2*3_600_000)
	stopAt, hang := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(hang) })
	r.av.mu.Lock()
	r.av.onCall = func(n int) error {
		if n == 3 {
			close(stopAt)
			<-hang // the third upload never answers
		}
		return nil
	}
	r.av.mu.Unlock()
	// The fake upload ignores its context; this one gives up when it ends.
	r.m.asr = ctxASR{r.av}
	r.hook.push("make", r.req)
	<-stopAt
	r.stop()
	r.hook.noAnswer(50 * time.Millisecond)
	work := r.m.cache.workDir(madeID(r.req.FileID, r.req.Key))
	got, _ := filepath.Glob(filepath.Join(work, "chunk-*.json"))
	if len(got) != 2 {
		t.Errorf("chunks kept: %v, want the first 2", got)
	}
	left, _ := filepath.Glob(filepath.Join(r.m.dataDir, windowPattern))
	if len(left) != 0 {
		t.Errorf("audio files left: %v", left)
	}
}

// ctxASR runs an upload until it ends or ctx does.
type ctxASR struct{ f *filmAV }

func (c ctxASR) TranscribeChunk(ctx context.Context, path, lang string) (Transcript, error) {
	type res struct {
		tr  Transcript
		err error
	}
	ch := make(chan res, 1)
	go func() { tr, err := c.f.TranscribeChunk(ctx, path, lang); ch <- res{tr, err} }()
	select {
	case r := <-ch:
		return r.tr, r.err
	case <-ctx.Done():
		return Transcript{}, ctx.Err()
	}
}

func TestSiteBadJobs(t *testing.T) {
	r := newSiteRig(t, 300_000)
	if a := r.hook.answer(r.hook.push("translate", map[string]any{})); !strings.Contains(a.Error, `does not know the "translate" job`) {
		t.Errorf("unknown op: %+v", a)
	}
	if a := r.hook.answer(r.hook.push("make", map[string]any{"key": "x"})); a.Error != "the make could not be read" {
		t.Errorf("make without a file: %+v", a)
	}
	if a := r.hook.answer(r.hook.push("search", "not an object")); a.Error != "the search could not be read" {
		t.Errorf("bad search: %+v", a)
	}
	// A refused make answers its reason, short enough for HolyArrs.
	req := r.req
	req.Language = "fr"
	a := r.hook.answer(r.hook.push("make", req))
	if !strings.Contains(a.Error, "offer was for en") || len(a.Error) > maxAnswerError {
		t.Errorf("refused make: %+v", a)
	}
	if r.av.calls != 0 {
		t.Error("audio sent for a refused make")
	}
}

// The app runs the site only when transcribe is on and an address is set,
// and a whole film never reaches Whisper during a search.
func TestAppRunsSite(t *testing.T) {
	hook := newFakeHook(t)
	var whisperHits atomic.Int32
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		whisperHits.Add(1)
		http.Error(w, "no", http.StatusTeapot)
	}))
	t.Cleanup(whisper.Close)
	ha, _ := newHolyArrs(hook.srv.URL, "tok")
	for _, c := range []struct {
		transcribe bool
		addr       string
		site       bool
	}{{true, whisper.URL, true}, {false, whisper.URL, false}, {true, "", false}} {
		set := defaults()
		set.Transcribe, set.ASRURL = c.transcribe, c.addr
		a, err := newApp(t.TempDir(), set, ha, library(t), &syncBuf{})
		if err != nil {
			t.Fatal(err)
		}
		if (a.site != nil) != c.site {
			t.Errorf("%+v: site %t", c, a.site != nil)
		}
	}
	set := defaults()
	set.ASRURL = whisper.URL
	a, _ := newApp(t.TempDir(), set, ha, library(t), &syncBuf{})
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw, _ := os.Pipe()
	done := make(chan error)
	go func() { done <- a.run(ctx, pr) }()
	ans := hook.answer(hook.push("search", searchAsk{Kind: "movie", Languages: []string{"en"}, FileID: 311,
		Folder: "movies", Path: "Arrival (2016)/Arrival (2016).mkv"}))
	if string(ans.Data) != "[]" { // an empty file is not a film
		t.Errorf("search: %+v", ans)
	}
	cancel()
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	pr.Close()
	if whisperHits.Load() != 0 {
		t.Error("search reached Whisper")
	}
}
