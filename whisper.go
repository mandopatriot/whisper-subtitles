package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The Whisper Pro ASR call, as probed on 2026-11-25 (version 1.5.1). Word
// timings are asked for; engines that give them (Faster-Whisper based, and
// PARAKEET) add "words" to each segment, while INTEL-WHISPER answers with
// segments only. The "text" field holds an SRT with an injected "Made with
// Whisper Pro ASR" cue, so it is never read. The audio track's language is
// sent when the track is tagged (asrQueryFor), which skips Whisper's
// language detection: on a near-silent window that detection hung the
// server.
const asrQuery = "output=json&task=transcribe&encode=true&vad_filter=true&word_timestamps=true"

// asrQueryFor adds language=lang (a two- or three-letter code from
// media.BaseLanguage) when it is set.
func asrQueryFor(lang string) string {
	if lang == "" {
		return asrQuery
	}
	return asrQuery + "&language=" + lang
}

// validLang is what asrQueryFor accepts: lower-case letters only.
func validLang(lang string) bool {
	if len(lang) < 2 || len(lang) > 3 {
		return false
	}
	for _, r := range lang {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// Limits on a Whisper answer.
const (
	maxASRBody     = 16 << 20
	maxSegments    = 20_000
	maxSegmentText = 4 << 10
	maxWords       = 200_000 // in one answer
	maxWordText    = 256
	asrRetryWait   = 15 * time.Second
	// asrUploadTimeout is one window's whole call, queue included: 20 times
	// the usual 15 to 30 s for 90 s of audio. A near-silent window once hung
	// the server in language detection; a call over this is not retried.
	asrUploadTimeout = 10 * time.Minute
	// asrChunkTimeout is one transcription chunk's call: up to 2 min of
	// audio, which INTEL-WHISPER decodes in about 15 s.
	asrChunkTimeout = 10 * time.Minute
)

// Whisper calls the owner's Whisper Pro ASR server through the plugin's
// proxy (HTTP_PROXY), which lets the owner host and its port through.
type Whisper struct {
	Base      string // asr_url, such as http://whisper.example.lan:9000
	HTTP      *http.Client
	RetryWait time.Duration
	// Logf, when set, logs an upload sent again after a dropped connection,
	// named by uploadName(ctx).
	Logf func(string, ...any)
}

type uploadKey struct{}

// withUploadName names the upload made with ctx in the log, such as a
// sync window or a make chunk.
func withUploadName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, uploadKey{}, name)
}

func uploadName(ctx context.Context) string {
	if s, _ := ctx.Value(uploadKey{}).(string); s != "" {
		return s
	}
	return "an upload"
}

func newWhisper(base string) *Whisper {
	return &Whisper{Base: base, RetryWait: asrRetryWait,
		HTTP: &http.Client{Timeout: asrUploadTimeout, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}}
}

// Segment is one stretch of speech, in seconds from the start of the upload.
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
	Words []Word  `json:"words"` // only from engines that give word timings
	// NoSpeechProb and AvgLogprob are Whisper's own confidence, when the
	// engine sends them (nil otherwise). Only transcription reads them.
	NoSpeechProb *float64 `json:"no_speech_prob"`
	AvgLogprob   *float64 `json:"avg_logprob"`
}

// Word is one word's timing within a segment, in seconds from the start of
// the upload.
type Word struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Word  string  `json:"word"`
}

// Transcript is what Whisper heard in one window.
type Transcript struct {
	Language    string  // detected, such as en
	Probability float64 // of that language, 0 to 1
	Duration    float64 // seconds of audio Whisper decoded
	Segments    []Segment
	Overruns    int  // segments dropped for running past the end of the audio
	Words       bool // the segments carry word timings
}

// overrunSlackSec is how far past the audio a segment may end and still be
// kept. Whisper's last segment in a window often ends about 30 s past the
// audio (its decoding window), such as 88 to 117.98 in 90 s; that segment's
// timing is unreliable, so it is dropped and the rest kept.
const overrunSlackSec = 5

// ErrASR marks an answer the plugin can't use; the subtitle is left alone.
var ErrASR = errors.New("whisper")

// errASRTimeout is an upload that got no answer within the time limit. It
// is never retried: the server may still be working on it, or be stuck.
var errASRTimeout = fmt.Errorf("%w: no answer within the upload time limit", ErrASR)

// errASRDown is a server that can't be reached: a refused connection, a
// name that does not resolve, or a 502, 503 or 504 from a proxy in front of
// it. Uploads wait until GET /asr answers again (asrQueue.send) instead of
// failing.
var errASRDown = fmt.Errorf("%w: the server can't be reached", ErrASR)

// errASRGateway is a 502, 503 or 504 from a proxy in front of the server.
// It is errASRDown, unless GET /asr answers at once: then the server is up
// but busy, and the upload counts as a timeout (asrQueue.send).
var errASRGateway error = gatewayError{}

type gatewayError struct{}

func (gatewayError) Error() string   { return "whisper: a proxy in front of the server answered" }
func (gatewayError) Is(t error) bool { return t == errASRDown || t == ErrASR }

// errASRLost is a connection lost during an upload (reset, EOF), and
// errASRForbidden a 403. Through HolyArrs's proxy a stopped server looks
// like either: the proxy can't connect, answers 403 with an empty body and
// closes. After one, GET /asr is tried (asrQueue.send): if it fails too, the
// server is away.
var (
	errASRLost      error = markErr("whisper: the connection was lost")
	errASRForbidden error = markErr("whisper: 403 Forbidden")
)

type markErr string

func (e markErr) Error() string   { return string(e) }
func (e markErr) Is(t error) bool { return t == ErrASR }

// isUnreachable says whether err happened before the server took the
// upload: dialling the server or the proxy, or looking up its name.
func isUnreachable(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && (op.Op == "dial" || op.Op == "proxyconnect")
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (w *Whisper) logf(f string, a ...any) {
	if w.Logf != nil {
		w.Logf(f, a...)
	}
}

// Ready checks GET /asr, which answers "ready".
func (w *Whisper) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.Base+"/asr", nil)
	if err != nil {
		return err
	}
	resp, err := w.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s is not reachable: %v", ErrASR, w.Base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK || !strings.Contains(strings.ToLower(string(b)), "ready") {
		return fmt.Errorf("%w: %s/asr answered %d %q", ErrASR, w.Base, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Transcribe uploads the audio file at path and reads the segments; lang is
// the audio track's language ("" when the track is untagged). A server that
// can't be reached is errASRDown, and one that took the upload and gave no
// answer in time is errASRTimeout, both returned at once. A connection lost
// during the upload is tried once more after RetryWait; any other failure is
// returned at once.
func (w *Whisper) Transcribe(ctx context.Context, path, lang string) (Transcript, error) {
	return w.transcribe(ctx, w.HTTP, path, lang)
}

// TranscribeChunk is Transcribe for a transcription chunk, with its longer
// time limit (asrChunkTimeout). The language is required.
func (w *Whisper) TranscribeChunk(ctx context.Context, path, lang string) (Transcript, error) {
	if lang == "" {
		return Transcript{}, fmt.Errorf("%w: a transcription needs the audio's language", ErrASR)
	}
	c := *w.HTTP
	c.Timeout = asrChunkTimeout
	return w.transcribe(ctx, &c, path, lang)
}

func (w *Whisper) transcribe(ctx context.Context, hc *http.Client, path, lang string) (Transcript, error) {
	if lang != "" && !validLang(lang) {
		return Transcript{}, fmt.Errorf("%w: language %q is not a language code", ErrASR, lang)
	}
	tr, retry, err := w.transcribeOnce(ctx, hc, path, lang)
	if err == nil || !retry {
		return tr, err
	}
	w.logf("%s: the connection to Whisper dropped (%v); sending it again in %s", uploadName(ctx), err, w.RetryWait)
	select {
	case <-ctx.Done():
		return Transcript{}, ctx.Err()
	case <-time.After(w.RetryWait):
	}
	tr, _, err = w.transcribeOnce(ctx, hc, path, lang)
	if err != nil && ctx.Err() == nil {
		w.logf("%s: sent again, it failed too: %v", uploadName(ctx), err)
	}
	return tr, err
}

func (w *Whisper) transcribeOnce(ctx context.Context, hc *http.Client, path, lang string) (tr Transcript, retry bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return tr, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return tr, false, err
	}
	// The multipart body is the file between a fixed head and tail, so its
	// length is known and the upload is never held in memory.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	head, tail, err := multipartFrame(mw.Boundary(), filepath.Base(path))
	if err != nil {
		return tr, false, err
	}
	go func() {
		_, err := io.Copy(pw, io.MultiReader(strings.NewReader(head), f, strings.NewReader(tail)))
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.Base+"/asr?"+asrQueryFor(lang), pr)
	if err != nil {
		pr.Close()
		return tr, false, err
	}
	req.ContentLength = int64(len(head)) + fi.Size() + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	pr.Close()
	if err != nil {
		if ctx.Err() != nil {
			return tr, false, ctx.Err()
		}
		if isUnreachable(err) {
			return tr, false, fmt.Errorf("%w (%s): %v", errASRDown, w.Base, err)
		}
		if isTimeout(err) {
			return tr, false, fmt.Errorf("%w (%s): %v", errASRTimeout, hc.Timeout, err)
		}
		return tr, true, fmt.Errorf("%w: upload to %s failed: %v", errASRLost, w.Base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxASRBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return tr, false, ctx.Err()
		}
		if isTimeout(err) {
			return tr, false, fmt.Errorf("%w (%s): %v", errASRTimeout, hc.Timeout, err)
		}
		return tr, true, fmt.Errorf("%w: reading the answer: %v", errASRLost, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		switch resp.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return tr, false, fmt.Errorf("%w %d for %s: %s", errASRGateway, resp.StatusCode, w.Base, msg)
		case http.StatusForbidden:
			return tr, false, fmt.Errorf("%w from %s: %q", errASRForbidden, w.Base, msg)
		}
		return tr, false, fmt.Errorf("%w: %s answered %d: %s", ErrASR, w.Base, resp.StatusCode, msg)
	}
	if len(body) > maxASRBody {
		return tr, false, fmt.Errorf("%w: the answer is over %d bytes", ErrASR, maxASRBody)
	}
	tr, err = parseTranscript(body)
	return tr, false, err
}

// multipartFrame returns the bytes before and after the file in a body with
// one part, audio_file, as the server's OpenAPI names it.
func multipartFrame(boundary, filename string) (head, tail string, err error) {
	if strings.ContainsAny(filename, "\"\r\n\\") {
		return "", "", fmt.Errorf("bad upload name %q", filename)
	}
	head = "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="audio_file"; filename="` + filename + "\"\r\n" +
		"Content-Type: audio/x-matroska\r\n\r\n"
	tail = "\r\n--" + boundary + "--\r\n"
	return head, tail, nil
}

// parseTranscript reads and checks an /asr?output=json answer.
func parseTranscript(body []byte) (Transcript, error) {
	var raw struct {
		Language    *string    `json:"language"`
		Probability *float64   `json:"language_probability"`
		Duration    *float64   `json:"video_duration_sec"`
		Segments    *[]Segment `json:"segments"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Transcript{}, fmt.Errorf("%w: the answer is not the expected JSON: %v", ErrASR, err)
	}
	if raw.Segments == nil || raw.Language == nil || raw.Probability == nil || raw.Duration == nil {
		return Transcript{}, fmt.Errorf("%w: the answer lacks segments, language, language_probability or video_duration_sec; check that the server is Whisper Pro ASR and output=json is honoured", ErrASR)
	}
	tr := Transcript{Language: strings.TrimSpace(*raw.Language), Probability: *raw.Probability, Duration: *raw.Duration, Segments: *raw.Segments}
	bad := func(f string, a ...any) (Transcript, error) {
		return Transcript{}, fmt.Errorf("%w: %s", ErrASR, fmt.Sprintf(f, a...))
	}
	switch {
	case !finite(tr.Probability) || tr.Probability < 0 || tr.Probability > 1:
		return bad("language_probability %v is not 0 to 1", tr.Probability)
	case !finite(tr.Duration) || tr.Duration < 0 || tr.Duration > 24*3600:
		return bad("video_duration_sec %v", tr.Duration)
	case len(tr.Segments) > maxSegments:
		return bad("%d segments, over %d", len(tr.Segments), maxSegments)
	case len(tr.Language) > 16:
		return bad("language %q", tr.Language)
	}
	kept := tr.Segments[:0]
	nWords := 0
	for i, s := range tr.Segments {
		if !finite(s.Start) || !finite(s.End) || s.Start < 0 || s.End < s.Start {
			return bad("segment %d runs %v to %v in %v s of audio", i, s.Start, s.End, tr.Duration)
		}
		// A confidence value out of its range is read as not sent, so it can
		// never drop a segment; the rest of the answer stays usable for sync.
		if p := s.NoSpeechProb; p != nil && (!finite(*p) || *p < 0 || *p > 1) {
			s.NoSpeechProb = nil
		}
		if p := s.AvgLogprob; p != nil && (!finite(*p) || *p > 0) {
			s.AvgLogprob = nil
		}
		if s.End > tr.Duration+overrunSlackSec {
			tr.Overruns++
			continue
		}
		if len(s.Text) > maxSegmentText {
			s.Text = s.Text[:maxSegmentText]
		}
		if nWords += len(s.Words); nWords > maxWords {
			return bad("over %d words", maxWords)
		}
		// A word with impossible times, or outside its segment by more
		// than the slack, is dropped; the segment keeps its other words.
		words := s.Words[:0]
		for _, wd := range s.Words {
			if !finite(wd.Start) || !finite(wd.End) || wd.Start < 0 || wd.End < wd.Start ||
				wd.Start < s.Start-overrunSlackSec || wd.End > s.End+overrunSlackSec {
				continue
			}
			if len(wd.Word) > maxWordText {
				wd.Word = wd.Word[:maxWordText]
			}
			words = append(words, wd)
		}
		s.Words = words
		if len(words) > 0 {
			tr.Words = true
		}
		kept = append(kept, s)
	}
	tr.Segments = kept
	return tr, nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// windowFile is a temporary .mka in the plugin's data folder, the only
// place it may write.
const windowPattern = "window-*.mka"

func newWindowFile(dataDir string) (*os.File, error) {
	return os.CreateTemp(dataDir, windowPattern)
}

// cleanWindowFiles removes window files a stop left behind.
func cleanWindowFiles(dataDir string) {
	matches, _ := filepath.Glob(filepath.Join(dataDir, windowPattern))
	for _, m := range matches {
		os.Remove(m)
	}
}
