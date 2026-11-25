package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseFixtures(t *testing.T) {
	for _, name := range []string{"whisper_asr_json.json", "whisper_v1_verbose.json"} {
		tr, err := parseTranscript(fixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tr.Language != "en" || tr.Probability != 1 || tr.Duration != 33.5146875 || len(tr.Segments) != 14 {
			t.Errorf("%s: %+v", name, tr)
		}
		if s := tr.Segments[0]; s.Start != 0 || s.End != 4 || s.Text == "" {
			t.Errorf("%s: first segment %+v", name, s)
		}
		for _, s := range tr.Segments {
			if strings.Contains(s.Text, "Whisper Pro ASR") {
				t.Errorf("%s: the injected watermark cue reached the segments", name)
			}
		}
	}
}

func TestParseRefusesBadAnswers(t *testing.T) {
	ok := `"language":"en","language_probability":0.9,"video_duration_sec":90`
	for name, body := range map[string]string{
		"not JSON":                       `<html>Bad Gateway</html>`,
		"SRT text only (no output=json)": `{"text":"1\n00:00:00,000 --> 00:00:03,000\nMade with Whisper Pro ASR"}`,
		"no segments":                    `{` + ok + `}`,
		"no language":                    `{"segments":[],"language_probability":1,"video_duration_sec":90}`,
		"probability over 1":             `{"segments":[],"language":"en","language_probability":1.5,"video_duration_sec":90}`,
		"negative duration":              `{"segments":[],"language":"en","language_probability":1,"video_duration_sec":-1}`,
		"duration over a day":            `{"segments":[],"language":"en","language_probability":1,"video_duration_sec":90000}`,
		"negative start":                 `{"segments":[{"start":-1,"end":2,"text":"a"}],` + ok + `}`,
		"end before start":               `{"segments":[{"start":5,"end":2,"text":"a"}],` + ok + `}`,
		"language too long":              `{"segments":[],"language":"` + strings.Repeat("x", 17) + `","language_probability":1,"video_duration_sec":90}`,
		"too many segments":              `{"segments":[` + strings.Repeat(`{"start":1,"end":1,"text":""},`, maxSegments) + `{"start":1,"end":1,"text":""}],` + ok + `}`,
	} {
		if _, err := parseTranscript([]byte(body)); !errors.Is(err, ErrASR) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	long := strings.Repeat("x", maxSegmentText+100)
	tr, err := parseTranscript([]byte(`{"segments":[{"start":1,"end":2,"text":"` + long + `"}],` + ok + `}`))
	if err != nil || len(tr.Segments[0].Text) != maxSegmentText {
		t.Errorf("long text: %v, %+v", err, tr.Segments)
	}
}

// A tagged track's language is sent, which skips Whisper's language
// detection; an untagged track sends none. Word timings are always asked for.
func TestTranscribeSendsLanguage(t *testing.T) {
	if !strings.Contains(asrQuery, "word_timestamps=true") || strings.Contains(asrQuery, "language=") {
		t.Fatalf("asrQuery %q", asrQuery)
	}
	fw := &fakeWhisper{t: t, query: asrQuery + "&language=en"}
	w := testWhisper(t, fw)
	if _, err := w.Transcribe(context.Background(), windowFile(t, []byte("mka")), "en"); err != nil {
		t.Fatal(err)
	}
	fw.query = asrQuery
	if _, err := w.Transcribe(context.Background(), windowFile(t, []byte("mka")), ""); err != nil {
		t.Fatal(err)
	}
	n := fw.calls.Load()
	for _, bad := range []string{"EN", "e", "engl", "en&x=1", "en-US", "é"} {
		if _, err := w.Transcribe(context.Background(), windowFile(t, []byte("mka")), bad); !errors.Is(err, ErrASR) {
			t.Errorf("language %q: %v, want refused", bad, err)
		}
	}
	if fw.calls.Load() != n {
		t.Errorf("a bad language reached the server")
	}
}

// PARAKEET's answer (probed 2026-11-25, dialogue replaced) carries words.
func TestParseWords(t *testing.T) {
	tr, err := parseTranscript(fixture(t, "whisper_parakeet_words.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Words || len(tr.Segments) != 4 || len(tr.Segments[0].Words) != 8 {
		t.Fatalf("%+v", tr)
	}
	if w := tr.Segments[0].Words[0]; w.Start != 17.388 || w.End != 17.708 || w.Word != " alpha" {
		t.Errorf("first word %+v", w)
	}
	// The segment-only fixture has no words.
	if tr, _ := parseTranscript(fixture(t, "whisper_asr_json.json")); tr.Words {
		t.Errorf("segments without words read as word timings")
	}
	// Bad words are dropped; their segment and the good words stay.
	ok := `"language":"en","language_probability":1,"video_duration_sec":90`
	tr, err = parseTranscript([]byte(`{"segments":[{"start":10,"end":12,"text":"a b c d e","words":[
		{"start":10,"end":10.5,"word":"a"},{"start":11,"end":10,"word":"b"},{"start":-1,"end":0,"word":"c"},
		{"start":4.9,"end":5,"word":"d"},{"start":11.5,"end":17.5,"word":"e"},{"start":11,"end":11.5,"word":"` + strings.Repeat("x", maxWordText+10) + `"}]}],` + ok + `}`))
	if err != nil || len(tr.Segments) != 1 || !tr.Words {
		t.Fatalf("%+v, %v", tr, err)
	}
	if ws := tr.Segments[0].Words; len(ws) != 2 || ws[0].Word != "a" || len(ws[1].Word) != maxWordText {
		t.Errorf("kept %+v; want a and the long word cut to %d", ws, maxWordText)
	}
	// Too many words refuse the answer.
	many := strings.Repeat(`{"start":1,"end":1,"word":"x"},`, maxWords) + `{"start":1,"end":1,"word":"x"}`
	if _, err := parseTranscript([]byte(`{"segments":[{"start":1,"end":2,"text":"x","words":[` + many + `]}],` + ok + `}`)); !errors.Is(err, ErrASR) {
		t.Errorf("%d words: %v, want refused", maxWords+1, err)
	}
}

// A segment ending past the audio (Whisper's last segment, start + ~30 s) is
// dropped and the rest kept; one ending within the slack is kept.
func TestParseDropsOverruns(t *testing.T) {
	ok := `"language":"en","language_probability":0.9,"video_duration_sec":90`
	tr, err := parseTranscript([]byte(`{"segments":[{"start":1,"end":2,"text":"a"},{"start":80,"end":95,"text":"b"},{"start":88,"end":117.98,"text":"c"},{"start":90,"end":95.01,"text":"d"}],` + ok + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Segments) != 2 || tr.Segments[0].Text != "a" || tr.Segments[1].Text != "b" || tr.Overruns != 2 {
		t.Errorf("got %+v, %d overruns; want a and b kept, 2 dropped", tr.Segments, tr.Overruns)
	}
}

// fakeWhisper answers /asr like the real server and records each upload.
type fakeWhisper struct {
	t      *testing.T
	calls  atomic.Int32
	status []int // answers in turn; then 200
	got    []byte
	absURI string // the request line's target, as a proxy sees it
	query  string // the query string wanted; asrQuery when empty
}

func (f *fakeWhisper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(f.calls.Add(1))
	f.absURI = r.RequestURI
	if r.Method == http.MethodGet && r.URL.Path == "/asr" {
		io.WriteString(w, "ready")
		return
	}
	want := f.query
	if want == "" {
		want = asrQuery
	}
	if r.Method != http.MethodPost || r.URL.Path != "/asr" || r.URL.RawQuery != want {
		f.t.Errorf("request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	}
	if r.ContentLength <= 0 || r.Header.Get("Content-Length") == "" || len(r.TransferEncoding) != 0 {
		f.t.Errorf("upload without a Content-Length (length %d, encoding %v)", r.ContentLength, r.TransferEncoding)
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		f.t.Errorf("multipart: %v", err)
	} else if fh := r.MultipartForm.File["audio_file"]; len(fh) != 1 || !strings.HasPrefix(fh[0].Filename, "window-") || fh[0].Header.Get("Content-Type") != "audio/x-matroska" {
		f.t.Errorf("parts %v", r.MultipartForm.File)
	} else {
		p, _ := fh[0].Open()
		f.got, _ = io.ReadAll(p)
		p.Close()
	}
	if n <= len(f.status) {
		http.Error(w, "busy", f.status[n-1])
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(fixture(f.t, "whisper_asr_json.json"))
}

func windowFile(t *testing.T, content []byte) string {
	t.Helper()
	f, err := newWindowFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.Write(content)
	f.Close()
	return f.Name()
}

func testWhisper(t *testing.T, fw *fakeWhisper) *Whisper {
	srv := httptest.NewServer(fw)
	t.Cleanup(srv.Close)
	w := newWhisper(srv.URL)
	w.RetryWait = 10 * time.Millisecond
	return w
}

func TestTranscribe(t *testing.T) {
	fw := &fakeWhisper{t: t}
	w := testWhisper(t, fw)
	audio := bytes.Repeat([]byte("mka!"), 5000)
	tr, err := w.Transcribe(context.Background(), windowFile(t, audio), "")
	if err != nil || len(tr.Segments) != 14 || tr.Language != "en" {
		t.Fatalf("%+v, %v", tr, err)
	}
	if !bytes.Equal(fw.got, audio) {
		t.Errorf("uploaded %d bytes, want the %d of the file", len(fw.got), len(audio))
	}
	if err := w.Ready(context.Background()); err != nil {
		t.Errorf("ready: %v", err)
	}
}

func TestTranscribeRetries(t *testing.T) {
	cases := []struct {
		status []int
		calls  int32
		ok     bool
	}{
		{[]int{503}, 2, true},
		{[]int{502}, 2, true},
		{[]int{504, 504}, 2, false},
		{[]int{400}, 1, false},
		{[]int{422}, 1, false},
		{[]int{500}, 1, false},
	}
	for _, c := range cases {
		fw := &fakeWhisper{t: t, status: c.status}
		w := testWhisper(t, fw)
		_, err := w.Transcribe(context.Background(), windowFile(t, []byte("mka")), "")
		if (err == nil) != c.ok || fw.calls.Load() != c.calls {
			t.Errorf("answers %v: err %v after %d calls; want ok %t after %d", c.status, err, fw.calls.Load(), c.ok, c.calls)
		}
		if err != nil && !errors.Is(err, ErrASR) {
			t.Errorf("answers %v: %v is not ErrASR", c.status, err)
		}
	}
}

func TestTranscribeConnectionRetry(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listens now
	w := newWhisper(base)
	w.RetryWait = 10 * time.Millisecond
	start := time.Now()
	_, err := w.Transcribe(context.Background(), windowFile(t, []byte("mka")), "")
	if !errors.Is(err, ErrASR) || time.Since(start) < 10*time.Millisecond {
		t.Errorf("refused connection: %v after %s; want one retry, then ErrASR", err, time.Since(start))
	}
}

func TestTranscribeStopsOnCancel(t *testing.T) {
	fw := &fakeWhisper{t: t, status: []int{503}}
	w := testWhisper(t, fw)
	w.RetryWait = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := w.Transcribe(ctx, windowFile(t, []byte("mka")), "")
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Errorf("cancel during the retry wait: %v after %s", err, time.Since(start))
	}
}

// TestTranscribeThroughProxy: the plugin's HTTP_PROXY gets the request in
// absolute form, naming the Whisper host and port, which is what the
// HolyArrs proxy reads to let an owner host through.
func TestTranscribeThroughProxy(t *testing.T) {
	fw := &fakeWhisper{t: t}
	proxy := httptest.NewServer(fw)
	defer proxy.Close()
	w := newWhisper("http://whisper.example.lan:9000")
	pu, _ := url.Parse(proxy.URL)
	w.HTTP.Transport.(*http.Transport).Proxy = http.ProxyURL(pu)
	if _, err := w.Transcribe(context.Background(), windowFile(t, []byte("mka")), ""); err != nil {
		t.Fatal(err)
	}
	if fw.absURI != "http://whisper.example.lan:9000/asr?"+asrQuery {
		t.Errorf("proxy saw %q", fw.absURI)
	}
}

func TestReadyRefusals(t *testing.T) {
	for _, h := range []http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "loading model") },
		func(w http.ResponseWriter, r *http.Request) { http.Error(w, "ready", 500) },
	} {
		srv := httptest.NewServer(h)
		if err := newWhisper(srv.URL).Ready(context.Background()); !errors.Is(err, ErrASR) {
			t.Errorf("ready: %v", err)
		}
		srv.Close()
	}
}

// TestUploadStreams: a 64 MiB window is sent from disk, not held in memory.
func TestUploadStreams(t *testing.T) {
	var got atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got.Store(n)
		w.Write(fixture(t, "whisper_asr_json.json"))
	}))
	defer srv.Close()
	f, _ := newWindowFile(t.TempDir())
	f.Truncate(64 << 20) // sparse: no disk used
	f.Close()
	w := newWhisper(srv.URL)
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	if _, err := w.Transcribe(context.Background(), f.Name(), ""); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&b)
	if got.Load() < 64<<20 {
		t.Errorf("server got %d bytes", got.Load())
	}
	if n := b.TotalAlloc - a.TotalAlloc; n > 8<<20 {
		t.Errorf("a 64 MiB upload allocated %s bytes", strconv.FormatUint(n, 10))
	}
}

func TestWindowFiles(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "state.json")
	os.WriteFile(keep, []byte("{}"), 0o600)
	for range 3 {
		f, err := newWindowFile(dir)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	cleanWindowFiles(dir)
	left, _ := os.ReadDir(dir)
	if len(left) != 1 || left[0].Name() != "state.json" {
		t.Errorf("left %v, want only state.json", left)
	}
	if _, _, err := multipartFrame("b", `bad"name.mka`); err == nil {
		t.Error("a quote in the upload name must be refused")
	}
}
