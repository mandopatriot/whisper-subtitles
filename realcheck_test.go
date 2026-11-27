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
)

// TestRealCheck runs the whole check on one real video from $WHISPER_SUBTITLES_LIB
// (read only) with the Whisper server at $WHISPER_SUBTITLES_ASR_URL and a fake HolyArrs:
// the subtitle $WHISPER_SUBTITLES_E2E_SUB, shifted by $WHISPER_SUBTITLES_E2E_SHIFT_MS into
// a temporary folder, must come back as a sync undoing the shift within
// $WHISPER_SUBTITLES_E2E_TOL_MS (default 600, the phrase-timing bound). Skipped
// unless the video, subtitle and server are set.
func TestRealCheck(t *testing.T) {
	vid, sub, asr := os.Getenv("WHISPER_SUBTITLES_E2E_VIDEO"), os.Getenv("WHISPER_SUBTITLES_E2E_SUB"), os.Getenv("WHISPER_SUBTITLES_ASR_URL")
	if vid == "" || sub == "" || asr == "" {
		t.Skip("set WHISPER_SUBTITLES_E2E_VIDEO, WHISPER_SUBTITLES_E2E_SUB and WHISPER_SUBTITLES_ASR_URL")
	}
	lib := testLibrary(t)
	for _, p := range []string{vid, sub} {
		if a, _ := filepath.Abs(p); !within(a, lib) {
			t.Fatalf("%s is not under WHISPER_SUBTITLES_LIB %s", p, lib)
		}
	}
	shift, _ := strconv.ParseInt(os.Getenv("WHISPER_SUBTITLES_E2E_SHIFT_MS"), 10, 64)
	tol := int64(600)
	if v, err := strconv.ParseInt(os.Getenv("WHISPER_SUBTITLES_E2E_TOL_MS"), 10, 64); err == nil {
		tol = v
	}
	b, err := os.ReadFile(sub)
	if err != nil {
		t.Fatal(err)
	}
	cues, err := readCues(b, "srt")
	if err != nil {
		t.Fatal(err)
	}
	for i := range cues {
		cues[i].StartMs, cues[i].EndMs = max(cues[i].StartMs+shift, 0), max(cues[i].EndMs+shift, 0)
	}
	work := t.TempDir()
	shifted := filepath.Join(work, "sub.en.srt")
	writeSRT(t, shifted, cues)

	ft := &fakeTitle{}
	rel, score := "Film/Film.en.srt", 80
	ft.row = SubRow{ID: 700, FileID: 70, Language: "en", Place: "beside", RelativePath: &rel, Score: &score, Origin: "fetched", State: "present"}
	log := &syncBuf{}
	k := &checker{ha: ft.server(t), asr: newWhisper(asr), queue: newASRQueue(), dataDir: t.TempDir(), open: openVideo,
		logf: func(f string, a ...any) { io.WriteString(log, strings.TrimSpace(fmt.Sprintf(f, a...))+"\n") }}
	c := Candidate{TitleID: 7, SubtitleID: 700, FileID: 70, VideoRel: "Film/Film.mkv", SubRel: rel, Language: "en", Format: "srt", Score: &score}
	err = k.check(context.Background(), c, Located{Video: vid, Sub: shifted})
	t.Logf("log:\n%s", log)
	if err != nil {
		t.Fatal(err)
	}
	f := (&checkRig{ft: ft}).syncBody(t)
	if d := f.OffsetMS + shift; d < -tol || d > tol || f.FromFPS != 0 {
		t.Errorf("sent %s for a %+d ms shift; want %+d within %d ms", f, shift, -shift, tol)
	}
	if m, _ := filepath.Glob(filepath.Join(k.dataDir, "*")); len(m) != 0 {
		t.Errorf("left in the data folder: %v", m)
	}
}
