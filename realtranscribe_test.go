package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"whisper-subtitles/media"
)

// shortVideo is a video cut to its first ms for a quicker real run.
type shortVideo struct {
	video
	ms int64
}

func (v shortVideo) Duration() int64 { return min(v.video.Duration(), v.ms) }

// TestRealTranscribe writes a subtitle for $WHISPER_SUBTITLES_FILE (a path
// inside $WHISPER_SUBTITLES_LIB, read only) with the server at
// $WHISPER_SUBTITLES_ASR_URL, as a make does: search, then make. With
// $WHISPER_SUBTITLES_MINUTES set, only that many minutes are written. The
// SRT goes to $WHISPER_SUBTITLES_OUT when set (never inside the library or a
// folder in $WHISPER_SUBTITLES_PROTECT), else to a temporary folder. Skipped
// unless all three are set.
func TestRealTranscribe(t *testing.T) {
	lib := testLibrary(t)
	rel, base := os.Getenv("WHISPER_SUBTITLES_FILE"), os.Getenv("WHISPER_SUBTITLES_ASR_URL")
	if rel == "" || base == "" {
		t.Skip("set WHISPER_SUBTITLES_FILE and WHISPER_SUBTITLES_ASR_URL")
	}
	limit := int64(0)
	if m := os.Getenv("WHISPER_SUBTITLES_MINUTES"); m != "" {
		n, err := strconv.Atoi(m)
		if err != nil || n <= 0 {
			t.Fatalf("WHISPER_SUBTITLES_MINUTES=%q", m)
		}
		limit = int64(n) * 60_000
	}
	out := os.Getenv("WHISPER_SUBTITLES_OUT")
	if out == "" {
		out = t.TempDir()
	}
	writableOut(t, out, lib)
	w := newWhisper(base)
	if err := w.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	open := func(p string) (video, io.Closer, error) {
		v, c, err := openVideo(p)
		if err == nil && limit > 0 {
			v = shortVideo{v, limit}
		}
		return v, c, err
	}
	folders := []Folder{{"movies", lib}}
	logf := func(f string, a ...any) { t.Logf(f, a...) }
	s := &searcher{set: Settings{Transcribe: true, ASRURL: base}, folders: folders, tracks: newTrackCache(open), wait: searchWait, logf: logf}
	lang := os.Getenv("WHISPER_SUBTITLES_LANGUAGE")
	if lang == "" {
		lang = "en"
	}
	offers := s.search(context.Background(), searchAsk{Kind: "movie", Languages: []string{lang}, FileID: 1, Folder: "movies", Path: rel})
	if len(offers) != 1 {
		t.Fatalf("offers %+v", offers)
	}
	data := t.TempDir()
	m := &maker{dataDir: data, folders: folders, open: open, asr: w, queue: newASRQueue(), cache: newMadeCache(data), logf: logf}
	t0 := time.Now()
	body, err := m.make(context.Background(), makeReq{Key: offers[0].Key, Language: lang, FileID: 1, Folder: "movies", Path: rel})
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(t0)
	cues := parseSRTBack(t, body)
	checkCueLimits(t, cues)
	p := filepath.Join(out, strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))+"."+lang+".whisper.srt")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	mins := float64(cues[len(cues)-1].EndMs) / 60_000
	fmt.Printf("%s: %d lines over %.1f min in %s (%s); written to %s\n", rel, len(cues), mins, took.Round(time.Second), media.BaseLanguage(lang), p)
	if per := float64(len(cues)) / max(mins, 1); per < 1 {
		t.Errorf("%.2f lines a minute: is the language right?", per)
	}
}
