package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"whisper-subtitles/media"
)

// evalWindow is one cached window transcript, next to its .mka as .json.
type evalWindow struct {
	Mka     string
	Source  string // the video's file name
	Lang    string // the audio track's language tag
	BaseMs  int64
	Tr      Transcript
	Err     string
	Seconds float64 // the round trip
}

// readEvalIndex reads TestRealFiles' output lines: path, source, codec,
// language, base=, dur=, ...
func readEvalIndex(path string) ([]evalWindow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []evalWindow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(sc.Text(), "\t")
		if len(p) < 6 || !strings.HasSuffix(p[0], ".mka") || !strings.HasPrefix(p[4], "base=") {
			continue
		}
		b, err := strconv.ParseInt(strings.TrimPrefix(p[4], "base="), 10, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, evalWindow{Mka: p[0], Source: p[1], Lang: p[3], BaseMs: b})
	}
	return out, sc.Err()
}

// evalCache is where a window's transcript is cached: <window>.json, or
// <window>.<tag>.json with $WHISPER_SUBTITLES_EVAL_TAG set (one tag per server).
func evalCache(mka string) string {
	if tag := os.Getenv("WHISPER_SUBTITLES_EVAL_TAG"); tag != "" {
		return mka + "." + tag + ".json"
	}
	return mka + ".json"
}

// TestTranscribeWindows transcribes every window listed in
// $WHISPER_SUBTITLES_EVAL_INDEX with the server at $WHISPER_SUBTITLES_ASR_URL, sending the
// track's language as the plugin does, and caches each answer. Windows
// already cached are skipped. It is skipped unless both are set.
func TestTranscribeWindows(t *testing.T) {
	index, base := os.Getenv("WHISPER_SUBTITLES_EVAL_INDEX"), os.Getenv("WHISPER_SUBTITLES_ASR_URL")
	if index == "" || base == "" {
		t.Skip("set WHISPER_SUBTITLES_EVAL_INDEX and WHISPER_SUBTITLES_ASR_URL")
	}
	wins, err := readEvalIndex(index)
	if err != nil || len(wins) == 0 {
		t.Fatalf("index: %d windows, %v", len(wins), err)
	}
	w := newWhisper(base)
	w.HTTP.Timeout = 10 * time.Minute
	if err := w.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, ew := range wins {
		cache := evalCache(ew.Mka)
		if _, err := os.Stat(cache); err == nil {
			continue
		}
		t0 := time.Now()
		tr, err := w.Transcribe(context.Background(), ew.Mka, media.BaseLanguage(ew.Lang))
		ew.Seconds = time.Since(t0).Seconds()
		ew.Tr = tr
		if err != nil {
			ew.Err = err.Error()
		}
		b, _ := json.Marshal(ew)
		if err := os.WriteFile(cache, b, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Printf("%d/%d %s: %s %.2f, %d segments, words %t, %.1f s, err %q\n", i+1, len(wins), ew.Mka, tr.Language, tr.Probability, len(tr.Segments), tr.Words, ew.Seconds, ew.Err)
	}
}
