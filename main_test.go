package main

import (
	"bytes"
	"context"
	"errors"
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

func TestRunJobTimeout(t *testing.T) {
	dir := t.TempDir()
	a, log := testApp(t, readySettings(), dir)
	a.jobTimeout = 50 * time.Millisecond
	a.check = func(ctx context.Context, c Candidate, loc Located) error {
		<-ctx.Done()
		return ctx.Err()
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- a.run(context.Background(), pr) }()
	io.WriteString(pw, arrivalLine)
	waitFor(t, "the timed-out job to be dropped", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.st.Highest == 5120 && len(a.st.Pending) == 0
	})
	pw.Close()
	<-done
	if !strings.Contains(log.String(), "time limit") {
		t.Errorf("log does not name the time limit:\n%s", log)
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
