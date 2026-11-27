package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// titleJSON is a GET /api/v1/titles/{id}/subtitles answer as HolyArrs writes
// it: the subtitle id comes as "ID" (an embedded struct without a JSON tag).
const titleJSON = `{"wishes":[],"files":[
 {"file_id":311,"item_id":5,"version_id":3,"version_label":"","relative_path":"Arrival (2016)/Arrival (2016).mkv","wish":{},
  "subtitles":[
   {"ID":9001,"CreatedAt":"2026-11-25T08:00:00Z","file_id":311,"language":"en","forced":false,"hearing_impaired":false,"place":"beside","relative_path":"Arrival (2016)/Arrival (2016).en.srt","site":"OpenSubtitles.com","score":85,"score_parts":[],"origin":"fetched","state":"present","sync":{},"tries":0},
   {"ID":9000,"file_id":311,"language":"en","place":"beside","relative_path":"Arrival (2016)/Arrival (2016).en.srt","score":70,"score_parts":[],"origin":"fetched","state":"replaced","sync":{},"tries":0},
   {"ID":9002,"file_id":311,"language":"es","place":"beside","relative_path":"Arrival (2016)/Arrival (2016).es.srt","score_parts":[],"origin":"found","state":"present","sync":{},"tries":0},
   {"ID":9003,"file_id":311,"language":"fr","place":"built_in","track_index":3,"codec":"subrip","score_parts":[],"origin":"found","state":"present","sync":{},"tries":0},
   {"ID":9004,"file_id":311,"language":"de","place":"wish","score_parts":[],"origin":"fetched","state":"wanted","sync":null,"tries":2},
   {"ID":9005,"file_id":311,"language":"pt-BR","place":"beside","relative_path":"Arrival (2016)/Arrival (2016).pt-BR.ass","score":60,"score_parts":[],"origin":"by_hand","state":"present","sync":{"offset_ms":1500,"from_subtitle_id":8000},"tries":0},
   {"ID":9006,"file_id":311,"language":"it","place":"beside","relative_path":"Arrival (2016)/Arrival (2016).it.sub","score":50,"score_parts":[],"origin":"fetched","state":"present","sync":{},"tries":0},
   {"ID":9007,"file_id":311,"language":"nl","place":"beside","relative_path":"Arrival (2016)/Arrival (2016).nl.vtt","score_parts":[],"origin":"by_hand","state":"present","sync":{},"tries":0}
  ]}]}`

func titleFiles(t *testing.T) []FileView {
	t.Helper()
	var out struct{ Files []FileView }
	if err := json.Unmarshal([]byte(titleJSON), &out); err != nil {
		t.Fatal(err)
	}
	return out.Files
}

func defaults() Settings {
	return Settings{ThresholdEpisodes: 96, ThresholdMovies: 86, Jobs: 1, JobTimeoutMinutes: 180, Transcribe: true, Sync: true}
}

func TestDecodeTitleSubtitles(t *testing.T) {
	f := titleFiles(t)
	if len(f) != 1 || f[0].FileID != 311 || f[0].RelativePath != "Arrival (2016)/Arrival (2016).mkv" || len(f[0].Subtitles) != 8 {
		t.Fatalf("decoded %+v", f)
	}
	r := f[0].Subtitles[0]
	if r.ID != 9001 || r.Score == nil || *r.Score != 85 || r.Origin != "fetched" || r.State != "present" || len(r.Sync) != 0 {
		t.Errorf("row 0 = %+v", r)
	}
}

func TestPickForEvent(t *testing.T) {
	files := titleFiles(t)
	j := Job{EventID: 1, TitleID: 42, Kind: "movie", FileID: 311, Path: "Arrival (2016)/Arrival (2016).en.srt"}
	c, err := pickForEvent(files, j, defaults())
	if err != nil {
		t.Fatal(err)
	}
	// The present row is picked, not the replaced one at the same path.
	want := Candidate{SubtitleID: 9001, FileID: 311, VideoRel: "Arrival (2016)/Arrival (2016).mkv",
		SubRel: "Arrival (2016)/Arrival (2016).en.srt", Language: "en", Format: "srt"}
	if c.Score == nil || *c.Score != 85 {
		t.Errorf("score = %v", c.Score)
	}
	c.Score = nil
	if c != want {
		t.Errorf("got %+v, want %+v", c, want)
	}
	for _, bad := range []Job{
		{TitleID: 42, Kind: "movie", FileID: 999, Path: j.Path},                                // file gone
		{TitleID: 42, Kind: "movie", FileID: 311, Path: "Arrival (2016)/Arrival (2016).x.srt"}, // subtitle gone
		{TitleID: 42, Kind: "movie", FileID: 311, Path: "Arrival (2016)/Arrival (2016).es.srt"},
		{TitleID: 42, Kind: "movie", FileID: 311, Path: "Arrival (2016)/Arrival (2016).pt-BR.ass"},
		{TitleID: 42, Kind: "movie", FileID: 311, Path: "Arrival (2016)/Arrival (2016).it.sub"},
	} {
		if _, err := pickForEvent(files, bad, defaults()); !errors.Is(err, errRule) {
			t.Errorf("%+v: err = %v, want a rule", bad, err)
		}
	}
}

func row(mut func(*SubRow)) SubRow {
	p, score := "A/A.en.srt", 50
	r := SubRow{ID: 1, Place: "beside", State: "present", RelativePath: &p, Origin: "fetched", Score: &score, Sync: map[string]any{}}
	if mut != nil {
		mut(&r)
	}
	return r
}

func made(m string) func(*SubRow) { return func(r *SubRow) { r.Made = &m } }

func TestCheckRowRules(t *testing.T) {
	set := defaults()
	score := func(n int) func(*SubRow) { return func(r *SubRow) { r.Score = &n } }
	path := func(p string) func(*SubRow) { return func(r *SubRow) { r.RelativePath = &p } }
	cases := []struct {
		name   string
		r      SubRow
		kind   string
		manual bool
		rule   string // "" means it passes
	}{
		{"fetched, low score", row(nil), "movie", false, ""},
		{"by_hand", row(func(r *SubRow) { r.Origin = "by_hand" }), "movie", false, ""},
		{"found: not saved by HolyArrs", row(func(r *SubRow) { r.Origin = "found" }), "movie", false, "did not save"},
		{"found, manual run too", row(func(r *SubRow) { r.Origin = "found" }), "", true, "did not save"},
		{"already synced", row(func(r *SubRow) { r.Sync = map[string]any{"offset_ms": 100.0} }), "movie", false, "already changed"},
		{"null sync is not synced", row(func(r *SubRow) { r.Sync = nil }), "movie", false, ""},
		{"built in", row(func(r *SubRow) { r.Place = "built_in" }), "movie", false, "not a file beside"},
		{"wish", row(func(r *SubRow) { r.Place = "wish" }), "movie", false, "not a file beside"},
		{"replaced", row(func(r *SubRow) { r.State = "replaced" }), "movie", false, "not present"},
		{"no path", row(func(r *SubRow) { r.RelativePath = nil }), "movie", false, "no path"},
		{".sub", row(path("A/A.en.sub")), "movie", false, ".sub"},
		{".sup", row(path("A/A.en.sup")), "movie", false, ".sup"},
		{".ASS upper case", row(path("A/A.en.ASS")), "movie", false, ""},
		{".vtt", row(path("A/A.en.vtt")), "movie", false, ""},
		{".ssa", row(path("A/A.en.ssa")), "movie", false, ""},
		{"episode 96 skipped", row(score(96)), "episode", false, "threshold 96"},
		{"episode 95 checked", row(score(95)), "episode", false, ""},
		{"movie 86 skipped", row(score(86)), "movie", false, "threshold 86"},
		{"movie 85 checked", row(score(85)), "movie", false, ""},
		{"movie 90 is under the episode threshold but skipped", row(score(90)), "movie", false, "threshold 86"},
		{"no score is checked", row(func(r *SubRow) { r.Score = nil }), "episode", false, ""},
		{"manual ignores score", row(score(100)), "", true, ""},
		{"manual keeps the sync rule", row(func(r *SubRow) { r.Sync = map[string]any{"offset_ms": 1.0} }), "", true, "already changed"},
		{"written from the audio", row(made("transcribed")), "movie", false, "written from the audio"},
		{"written from the audio, manual run too", row(made("transcribed")), "", true, "written from the audio"},
		{"translated is still checked", row(made("translated")), "movie", false, ""},
	}
	for _, c := range cases {
		err := checkRow(c.r, c.kind, set, c.manual)
		switch {
		case c.rule == "" && err != nil:
			t.Errorf("%s: %v, want it to pass", c.name, err)
		case c.rule != "" && (!errors.Is(err, errRule) || !strings.Contains(err.Error(), c.rule)):
			t.Errorf("%s: %v, want a rule naming %q", c.name, err, c.rule)
		}
	}
}

func TestCheckRowThresholdSettings(t *testing.T) {
	set := defaults()
	set.ThresholdMovies, set.ThresholdEpisodes = 101, 0
	if err := checkRow(row(func(r *SubRow) { n := 100; r.Score = &n }), "movie", set, false); err != nil {
		t.Errorf("threshold 101 must never skip: %v", err)
	}
	if err := checkRow(row(func(r *SubRow) { n := 0; r.Score = &n }), "episode", set, false); err == nil {
		t.Error("threshold 0 must skip every scored subtitle")
	}
}

func TestPickForTitle(t *testing.T) {
	got, skipped := pickForTitle(titleFiles(t), defaults())
	if len(got) != 2 || got[0].SubtitleID != 9001 || got[1].SubtitleID != 9007 || got[1].Format != "vtt" {
		t.Errorf("picked %+v, want 9001 and 9007", got)
	}
	// 9002 found, 9005 synced, 9006 .sub. The replaced row, the track inside
	// the video and the wish are not files to report.
	if len(skipped) != 3 || !strings.Contains(skipped[0], "9002") || !strings.Contains(skipped[1], "9005") || !strings.Contains(skipped[2], "9006") {
		t.Errorf("skipped %q", skipped)
	}
}
