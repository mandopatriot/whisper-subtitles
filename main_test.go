package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// syncBuf is a log the app and the test can share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// fakeHolyArrs answers GET /api/v1/titles/42/subtitles with titleJSON and
// refuses a missing or wrong token.
func fakeHolyArrs(t *testing.T) *HolyArrs {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"no token"}`, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/titles/42/subtitles" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, titleJSON)
			return
		}
		http.Error(w, `{"error":"no such Title"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	ha, err := newHolyArrs(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return ha
}

// library makes the Arrival files of titleJSON in a "movies" folder.
func library(t *testing.T) []Folder {
	t.Helper()
	lib := t.TempDir()
	touch(t, filepath.Join(lib, "Arrival (2016)/Arrival (2016).mkv"))
	touch(t, filepath.Join(lib, "Arrival (2016)/Arrival (2016).en.srt"))
	touch(t, filepath.Join(lib, "Arrival (2016)/Arrival (2016).nl.vtt"))
	return []Folder{{"movies", lib}}
}

func testApp(t *testing.T, set Settings, dataDir string) (*app, *syncBuf) {
	t.Helper()
	log := &syncBuf{}
	a, err := newApp(dataDir, set, fakeHolyArrs(t), library(t), log)
	if err != nil {
		t.Fatal(err)
	}
	return a, log
}

func readySettings() Settings {
	s := defaults()
	s.ASRURL = "http://whisper.test:9000"
	s.Transcribe = false // the subtitle site has its own tests (hook_test.go)
	return s
}

const arrivalLine = `{"event":"library.subtitle","id":5120,"payload":{"event_kind":"subtitle_saved","movie":{"id":42},"data":{"file_id":311,"language":"en","path":"Arrival (2016)/Arrival (2016).en.srt","score":85}}}` + "\n"

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunEventToCheck(t *testing.T) {
	dir := t.TempDir()
	a, log := testApp(t, readySettings(), dir)
	got := make(chan Located, 1)
	var seen Candidate
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		seen = c
		got <- loc
		return nil
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- a.run(context.Background(), pr) }()
	io.WriteString(pw, arrivalLine)
	loc := <-got
	if seen.SubtitleID != 9001 || loc.Folder != "movies" || !strings.HasSuffix(loc.Video, "Arrival (2016).mkv") || !strings.HasSuffix(loc.Sub, "Arrival (2016).en.srt") {
		t.Errorf("checked %+v at %+v", seen, loc)
	}
	waitFor(t, "the job to finish", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.st.Pending) == 0 })
	pw.Close() // EOF: HolyArrs stops the plugin
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, _ := loadState(dir)
	if st.Highest != 5120 || len(st.Pending) != 0 {
		t.Errorf("saved state %+v", st)
	}
	if !strings.Contains(log.String(), "queued event 5120") {
		t.Errorf("log:\n%s", log)
	}
}

func TestRunStopKeepsJobQueued(t *testing.T) {
	dir := t.TempDir()
	a, _ := testApp(t, readySettings(), dir)
	started := make(chan struct{})
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		close(started)
		<-ctx.Done() // a long check, cut off by the stop
		return ctx.Err()
	}
	ctx, stop := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	defer pw.Close()
	done := make(chan error, 1)
	go func() { done <- a.run(ctx, pr) }()
	io.WriteString(pw, arrivalLine)
	<-started
	stop() // SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, _ := loadState(dir)
	if len(st.Pending) != 1 || st.Pending[0].EventID != 5120 || st.Highest != 5120 {
		t.Errorf("after a stop mid-job: %+v; want the job still queued", st)
	}
	// The next start picks it up again, and the replayed line is skipped.
	b, log := testApp(t, readySettings(), dir)
	ran := make(chan struct{}, 1)
	b.check = func(ctx context.Context, c Candidate, loc Located) error { ran <- struct{}{}; return nil }
	pr2, pw2 := io.Pipe()
	done2 := make(chan error, 1)
	go func() { done2 <- b.run(context.Background(), pr2) }()
	io.WriteString(pw2, arrivalLine) // catch-up replays the same event
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatalf("queued job did not run after restart; log:\n%s", log)
	}
	waitFor(t, "the job to finish", func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.st.Pending) == 0 })
	pw2.Close()
	<-done2
	if !strings.Contains(log.String(), "already handled") {
		t.Errorf("replayed line was not skipped; log:\n%s", log)
	}
}

// TestStopEndsWorkers guards the 2026-11-25 OOM: after a stop, a worker took
// the same queued job again and again, logging each try, until the test
// process held 7 GB. A stop must end run promptly with one try only.
func TestStopEndsWorkers(t *testing.T) {
	for _, jobs := range []int{1, 4} {
		set := readySettings()
		set.Jobs = jobs
		a, log := testApp(t, set, t.TempDir())
		var tries atomic.Int32
		started := make(chan struct{}, 1)
		a.check = func(ctx context.Context, c Candidate, loc Located) error {
			tries.Add(1)
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		ctx, stop := context.WithCancel(context.Background())
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- a.run(ctx, pr) }()
		io.WriteString(pw, arrivalLine)
		<-started
		stop()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("jobs=%d: run did not return after the stop (%d tries)", jobs, tries.Load())
		}
		pw.Close()
		if n := tries.Load(); n != 1 {
			t.Errorf("jobs=%d: the stopped job was tried %d times, want 1", jobs, n)
		}
		if n := strings.Count(log.String(), "\n"); n > 10 {
			t.Errorf("jobs=%d: %d log lines after one job and a stop:\n%s", jobs, n, log)
		}
	}
}

func TestRunWaitsForASRAddress(t *testing.T) {
	dir := t.TempDir()
	a, _ := testApp(t, defaults(), dir) // no asr_url
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		t.Error("check ran without a Whisper address")
		return nil
	}
	if err := a.run(context.Background(), strings.NewReader(arrivalLine)); err != nil {
		t.Fatal(err)
	}
	st, _ := loadState(dir)
	if len(st.Pending) != 1 {
		t.Errorf("pending = %+v, want the job kept until the address is set", st.Pending)
	}
}

func TestRunJobsAtOnce(t *testing.T) {
	for _, n := range []int{1, 2} {
		set := readySettings()
		set.Jobs = n
		set.CheckTitlesRaw = "42"
		set.CheckTitles = []int64{42}
		dir := t.TempDir()
		a, _ := testApp(t, set, dir)
		// Two queued event jobs plus the check_titles job.
		a.st.push(Job{EventID: 1, TitleID: 42, Kind: "movie", FileID: 311, Path: "Arrival (2016)/Arrival (2016).en.srt"})
		a.st.push(Job{EventID: 2, TitleID: 42, Kind: "movie", FileID: 311, Path: "Arrival (2016)/Arrival (2016).nl.vtt"})
		var now, peak, calls atomic.Int32
		a.check = func(ctx context.Context, c Candidate, loc Located) error {
			if v := now.Add(1); v > peak.Load() {
				peak.Store(v)
			}
			time.Sleep(30 * time.Millisecond)
			now.Add(-1)
			calls.Add(1)
			return nil
		}
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- a.run(context.Background(), pr) }()
		waitFor(t, "all jobs", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.st.Pending) == 0 })
		pw.Close()
		<-done
		// Event 1 (en.srt), event 2 (nl.vtt), and check_titles on title 42 (both).
		if calls.Load() != 4 {
			t.Errorf("jobs=%d: %d checks, want 4", n, calls.Load())
		}
		if int(peak.Load()) > n {
			t.Errorf("jobs=%d: %d ran at once", n, peak.Load())
		}
		if n == 2 && peak.Load() != 2 {
			t.Errorf("jobs=2: never ran two at once")
		}
	}
}

// startApp runs a with no more event lines than given; stop ends it.
func startApp(t *testing.T, a *app, lines ...string) (stop func()) {
	t.Helper()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- a.run(context.Background(), pr) }()
	for _, l := range lines {
		io.WriteString(pw, l)
	}
	return func() {
		pw.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func pendingOf(a *app) []Job {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Job(nil), a.st.Pending...)
}

// A sync cut off by the time limit is tried again by itself, twice, each
// after a longer wait, and then given up.
func TestRunJobTimeoutRetries(t *testing.T) {
	if maxRetries != 2 || len(retryWaits) != 2 || retryWaits[0] != 30*time.Minute || retryWaits[1] != 2*time.Hour {
		t.Fatal("retries must be 2, after 30 min and then 2 h")
	}
	dir := t.TempDir()
	a, log := testApp(t, readySettings(), dir)
	a.jobTimeout = 50 * time.Millisecond
	a.retryWaits = []time.Duration{150 * time.Millisecond, 300 * time.Millisecond}
	var mu sync.Mutex
	var at []time.Time
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		<-ctx.Done()
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		return ctx.Err()
	}
	stop := startApp(t, a, arrivalLine)
	waitFor(t, "three tries", func() bool { mu.Lock(); defer mu.Unlock(); return len(at) == 3 })
	waitFor(t, "the queue to empty", func() bool { return len(pendingOf(a)) == 0 })
	stop()
	if g1, g2 := at[1].Sub(at[0]), at[2].Sub(at[1]); g1 < 150*time.Millisecond || g2 < 300*time.Millisecond {
		t.Errorf("tries %s and %s apart, want at least 150ms and 300ms", g1, g2)
	}
	l := log.String()
	for _, want := range []string{
		"event 5120, title 42, file 311, Arrival (2016)/Arrival (2016).en.srt: stopped at the time limit of 50ms",
		"event 5120, title 42, file 311, Arrival (2016)/Arrival (2016).en.srt, retry 1 of 2: tried again in 150ms",
		"retry 1 of 2: stopped at the time limit",
		"retry 2 of 2: tried again in 300ms",
		"not tried again, after 2 retries",
	} {
		if !strings.Contains(l, want) {
			t.Errorf("log lacks %q:\n%s", want, l)
		}
	}
	time.Sleep(400 * time.Millisecond)
	if mu.Lock(); len(at) != 3 {
		t.Errorf("%d tries, want 3", len(at))
	}
	mu.Unlock()
}

// A Whisper timeout queues a retry in state.json; it survives a restart
// and runs once, when due.
func TestRunWhisperTimeoutRetryAfterRestart(t *testing.T) {
	dir := t.TempDir()
	a, _ := testApp(t, readySettings(), dir)
	a.retryWaits = []time.Duration{300 * time.Millisecond, time.Hour}
	var calls atomic.Int32
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		calls.Add(1)
		return fmt.Errorf("%w (10m0s): context deadline exceeded", errASRTimeout)
	}
	stop := startApp(t, a, arrivalLine)
	waitFor(t, "the retry to be queued", func() bool { p := pendingOf(a); return len(p) == 1 && p[0].Tries == 1 })
	stop()
	st, _ := loadState(dir)
	if len(st.Pending) != 1 || st.Pending[0].Tries != 1 || st.Pending[0].NotBefore <= time.Now().UnixMilli() ||
		st.Pending[0].Path != "Arrival (2016)/Arrival (2016).en.srt" || st.Pending[0].EventID != 5120 {
		t.Fatalf("saved %+v, want the retry waiting", st.Pending)
	}
	due := time.UnixMilli(st.Pending[0].NotBefore)
	b, log := testApp(t, readySettings(), dir)
	var ranAt []time.Time
	var mu sync.Mutex
	b.check = func(ctx context.Context, c Candidate, loc Located) error {
		mu.Lock()
		ranAt = append(ranAt, time.Now())
		mu.Unlock()
		return nil
	}
	stop = startApp(t, b, arrivalLine) // the replayed event is skipped
	waitFor(t, "the retry", func() bool { return len(pendingOf(b)) == 0 })
	time.Sleep(100 * time.Millisecond)
	stop()
	if calls.Load() != 1 || len(ranAt) != 1 || ranAt[0].Before(due) {
		t.Errorf("first start %d tries, second %v (due %v); want one each, the retry when due", calls.Load(), ranAt, due)
	}
	if !strings.Contains(log.String(), "already handled") {
		t.Errorf("log:\n%s", log)
	}
}

// A subtitle checked and left alone, or failing for another reason, is not
// tried again.
func TestRunLeftAloneNotRetried(t *testing.T) {
	for _, e := range []error{
		fmt.Errorf("%w: no confident fit", errRule),
		fmt.Errorf("%w: the subtitle is in another language than the sound", errRule),
		fmt.Errorf("%w: server said 500", ErrASR),
	} {
		dir := t.TempDir()
		a, log := testApp(t, readySettings(), dir)
		a.retryWaits = []time.Duration{time.Millisecond, time.Millisecond}
		var calls atomic.Int32
		a.check = func(ctx context.Context, c Candidate, loc Located) error { calls.Add(1); return e }
		stop := startApp(t, a, arrivalLine)
		waitFor(t, "the job", func() bool { return calls.Load() == 1 && len(pendingOf(a)) == 0 })
		time.Sleep(50 * time.Millisecond)
		stop()
		if calls.Load() != 1 || strings.Contains(log.String(), "tried again") {
			t.Errorf("%v: %d tries; log:\n%s", e, calls.Load(), log)
		}
	}
}

// A check_titles run retries only the subtitle that was cut off, still
// without the score rule.
func TestRunCheckTitlesRetriesOne(t *testing.T) {
	dir := t.TempDir()
	set := readySettings()
	set.CheckTitlesRaw, set.CheckTitles = "42", []int64{42}
	a, log := testApp(t, set, dir)
	a.retryWaits = []time.Duration{50 * time.Millisecond, time.Hour}
	var mu sync.Mutex
	var seen []string
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		mu.Lock()
		seen = append(seen, c.SubRel)
		n := len(seen)
		mu.Unlock()
		if n == 1 && strings.HasSuffix(c.SubRel, ".en.srt") {
			return fmt.Errorf("%w: no answer", errASRTimeout)
		}
		return nil
	}
	stop := startApp(t, a)
	waitFor(t, "the retry", func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) == 3 })
	waitFor(t, "the queue to empty", func() bool { return len(pendingOf(a)) == 0 })
	stop()
	want := []string{"Arrival (2016)/Arrival (2016).en.srt", "Arrival (2016)/Arrival (2016).nl.vtt", "Arrival (2016)/Arrival (2016).en.srt"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Errorf("checked %v, want %v", seen, want)
	}
	if !strings.Contains(log.String(), "title 42, file 311, Arrival (2016)/Arrival (2016).en.srt (check_titles), retry 1 of 2: tried again in 50ms") {
		t.Errorf("log:\n%s", log)
	}
}

// Syncs waiting in state.json when the plugin stops are picked up at the
// next start, each run once, even with two at a time.
func TestRestartWithSyncsWaiting(t *testing.T) {
	dir := t.TempDir()
	en, nl := "Arrival (2016)/Arrival (2016).en.srt", "Arrival (2016)/Arrival (2016).nl.vtt"
	soon := time.Now().Add(300 * time.Millisecond)
	st := State{Highest: 5121, Pending: []Job{
		{EventID: 5120, TitleID: 42, Kind: "movie", FileID: 311, Path: en},
		{EventID: 5121, TitleID: 42, Kind: "movie", FileID: 311, Path: nl},
		{EventID: 5119, TitleID: 42, Kind: "movie", FileID: 311, Path: en, Tries: 1, NotBefore: soon.UnixMilli()},
	}}
	if err := st.save(dir); err != nil {
		t.Fatal(err)
	}
	set := readySettings()
	set.Jobs = 2
	a, log := testApp(t, set, dir)
	var mu sync.Mutex
	runs := map[string][]time.Time{}
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		runs[c.SubRel] = append(runs[c.SubRel], time.Now())
		mu.Unlock()
		return nil
	}
	stop := startApp(t, a, arrivalLine) // 5120 again, replayed: skipped
	waitFor(t, "every waiting sync", func() bool { return len(pendingOf(a)) == 0 })
	time.Sleep(100 * time.Millisecond)
	stop()
	if len(runs[en]) != 2 || len(runs[nl]) != 1 {
		t.Fatalf("ran %v; want en twice (event 5120 and the retry of 5119) and nl once", runs)
	}
	if last := runs[en][1]; last.Before(soon) {
		t.Errorf("the retry ran at %v, before it was due at %v", last, soon)
	}
	if !strings.Contains(log.String(), "already handled") {
		t.Errorf("log:\n%s", log)
	}
	// A third start finds nothing to do.
	b, _ := testApp(t, set, dir)
	var again atomic.Int32
	b.check = func(ctx context.Context, c Candidate, loc Located) error { again.Add(1); return nil }
	stop = startApp(t, b)
	time.Sleep(100 * time.Millisecond)
	stop()
	if again.Load() != 0 {
		t.Errorf("%d syncs ran again after a clean stop", again.Load())
	}
}

func TestCheckTitlesOnce(t *testing.T) {
	dir := t.TempDir()
	set := readySettings()
	set.CheckTitlesRaw, set.CheckTitles = "42", []int64{42}
	var calls atomic.Int32
	for range 2 { // a second start with the same saved value queues nothing
		a, _ := testApp(t, set, dir)
		a.check = func(ctx context.Context, c Candidate, loc Located) error { calls.Add(1); return nil }
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- a.run(context.Background(), pr) }()
		waitFor(t, "check_titles", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.st.Pending) == 0 })
		pw.Close()
		<-done
	}
	if calls.Load() != 2 { // en.srt and nl.vtt, once
		t.Errorf("%d checks over two starts, want 2", calls.Load())
	}
}

func TestRunLogsRulesAndMissingFolders(t *testing.T) {
	dir := t.TempDir()
	a, log := testApp(t, readySettings(), dir)
	a.folders = nil
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		t.Error("check ran without an approved folder")
		return nil
	}
	high := strings.Replace(arrivalLine, `"id":5120`, `"id":5200`, 1)
	high = strings.Replace(high, "Arrival (2016).en.srt", "Arrival (2016).es.srt", 1)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- a.run(context.Background(), pr) }()
	io.WriteString(pw, arrivalLine+high)
	waitFor(t, "both jobs", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.st.Highest == 5200 && len(a.st.Pending) == 0 })
	pw.Close()
	<-done
	out := log.String()
	if !strings.Contains(out, "no Library folder is approved") || !strings.Contains(out, "HolyArrs did not save it") {
		t.Errorf("log:\n%s", out)
	}
}

func TestHolyArrsErrors(t *testing.T) {
	ha := fakeHolyArrs(t)
	_, err := ha.TitleSubtitles(context.Background(), 7)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Msg != "no such Title" {
		t.Errorf("unknown title: %v", err)
	}
	ha.Token = "wrong"
	if _, err := ha.TitleSubtitles(context.Background(), 42); !errors.As(err, &ae) || ae.Status != 401 {
		t.Errorf("bad token: %v", err)
	}
	if _, err := newHolyArrs("127.0.0.1:6262", "t"); err == nil {
		t.Error("a target without a scheme must be refused")
	}
	if _, err := newHolyArrs("http://127.0.0.1:6262", ""); err == nil {
		t.Error("an empty token must be refused")
	}
	// HolyArrs is never reached through the plugin's proxy.
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:1")
	ha.Token = "tok"
	if _, err := ha.TitleSubtitles(context.Background(), 42); err != nil {
		t.Errorf("with HTTP_PROXY set: %v", err)
	}
}

// With sync off, events and check_titles queue nothing, but the event ids
// still move on so they are not checked later.
func TestRunSyncOff(t *testing.T) {
	dir := t.TempDir()
	set := readySettings()
	set.Sync = false
	set.CheckTitlesRaw, set.CheckTitles = "42", []int64{42}
	a, log := testApp(t, set, dir)
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		t.Errorf("checked %s with sync off", c)
		return nil
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- a.run(context.Background(), pr) }()
	io.WriteString(pw, arrivalLine)
	waitFor(t, "the event to be read", func() bool { return strings.Contains(log.String(), "sync is off; event 5120") })
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, _ := loadState(dir)
	if st.Highest != 5120 || len(st.Pending) != 0 || st.CheckTitlesDone != "" {
		t.Errorf("saved state %+v", st)
	}
	if !strings.Contains(log.String(), "check_titles: sync is off") {
		t.Errorf("log:\n%s", log)
	}
}
