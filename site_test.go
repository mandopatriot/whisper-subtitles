package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"whisper-subtitles/media"
)

func track(num uint64, lang, codec string, bps int64) media.Track {
	return media.Track{Number: num, Audio: true, Enabled: true, CodecID: codec, Language: lang, BPS: bps}
}

// tracksOnly is a video known only by its tracks, as search reads it.
type tracksOnly []media.Track

func (v tracksOnly) Duration() int64          { return 3_600_000 }
func (v tracksOnly) AllTracks() []media.Track { return v }
func (v tracksOnly) Extract(media.Track, int64, int64, int64, io.Writer) (media.Window, error) {
	return media.Window{}, errors.New("search must not read audio")
}

type searchRig struct {
	s     *searcher
	ask   searchAsk
	opens atomic.Int32
	log   *syncBuf
	fi    os.FileInfo
}

func newSearchRig(t *testing.T, tracks ...media.Track) *searchRig {
	t.Helper()
	lib := t.TempDir()
	p := filepath.Join(lib, "Arrival (2016)/Arrival (2016).mkv")
	touch(t, p)
	fi, _ := os.Stat(p)
	r := &searchRig{log: &syncBuf{}, fi: fi}
	set := defaults()
	set.ASRURL = "http://whisper.test:9000"
	open := func(string) (video, io.Closer, error) {
		r.opens.Add(1)
		return tracksOnly(tracks), io.NopCloser(nil), nil
	}
	r.s = &searcher{set: set, folders: []Folder{{"movies", lib}, {"shows", t.TempDir()}}, tracks: newTrackCache(open), wait: searchWait,
		logf: func(f string, a ...any) { fmt.Fprintf(r.log, f+"\n", a...) }}
	r.ask = searchAsk{Kind: "movie", Title: "Arrival", Languages: []string{"en"}, FileID: 4182, Folder: "movies", Path: "Arrival (2016)/Arrival (2016).mkv"}
	return r
}

func (r *searchRig) langs() []string {
	var out []string
	for _, o := range r.s.search(context.Background(), r.ask) {
		out = append(out, o.Language)
	}
	return out
}

func TestSearchOffers(t *testing.T) {
	r := newSearchRig(t,
		track(1, "eng", "A_TRUEHD", 0), track(2, "eng", "A_AC3", 0), // lossless and lossy English
		track(3, "fre", "A_AC3", 0),
		media.Track{Number: 4, Audio: true, Enabled: true, CodecID: "A_AC3", Language: "ger", Commentary: true},
		media.Track{Number: 5, Audio: true, Enabled: true, CodecID: "A_AC3", Language: "spa", Name: "Audio Description"},
		media.Track{Number: 6, Audio: true, Enabled: true, CodecID: "A_AC3", Language: "ita", Described: true},
		track(7, "und", "A_AAC", 0),
		track(8, "jpn", "A_PCM/INT/LIT", 20_000_000), // 64 MiB holds under 30 s (26.8 s)
		media.Track{Number: 9, Audio: true, Enabled: false, CodecID: "A_AC3", Language: "kor"},
	)
	r.ask.Languages = []string{"en", "fr", "de", "es", "it", "ja", "ko", "nl", "fr", "xx"}
	offers := r.s.search(context.Background(), r.ask)
	if len(offers) != 2 {
		t.Fatalf("offers %+v, want English and French", offers)
	}
	en, fr := offers[0], offers[1]
	want := offerKey{Track: 2, Lang: "en", Size: r.fi.Size(), MTime: r.fi.ModTime().Unix()}.String()
	if en.Language != "en" || en.Key != want || en.Format != "srt" || en.HearingImpaired || en.Forced {
		t.Errorf("English offer %+v, want the lossy track 2 as %s", en, want)
	}
	if fr.Language != "fr" || !strings.HasPrefix(fr.Key, "v1:t3:fr:") {
		t.Errorf("French offer %+v", fr)
	}
	for _, s := range []string{"no ja offer", "too large to send"} {
		if !strings.Contains(r.log.String(), s) {
			t.Errorf("log lacks %q:\n%s", s, r.log)
		}
	}
}

// Only an untagged track: no offer, since nobody checked its language.
func TestSearchUntaggedNoOffer(t *testing.T) {
	for _, lang := range []string{"und", ""} {
		r := newSearchRig(t, track(1, lang, "A_AAC", 0))
		if got := r.langs(); len(got) != 0 {
			t.Errorf("%q track: offers %v", lang, got)
		}
		if !strings.Contains(r.log.String(), "no language tag") {
			t.Errorf("log: %s", r.log)
		}
	}
}

// The ask's own language code comes back, so pt-BR stays pt-BR.
func TestSearchKeepsAskCode(t *testing.T) {
	r := newSearchRig(t, track(1, "por", "A_AAC", 0))
	r.ask.Languages = []string{"pt-BR"}
	o := r.s.search(context.Background(), r.ask)
	if len(o) != 1 || o[0].Language != "pt-BR" || !strings.Contains(o[0].Key, ":pt:") {
		t.Errorf("offers %+v", o)
	}
}

func TestSearchNoOffer(t *testing.T) {
	cases := []struct {
		name string
		edit func(r *searchRig)
		log  string
	}{
		{"transcribe off", func(r *searchRig) { r.s.set.Transcribe = false }, ""},
		{"no Whisper address", func(r *searchRig) { r.s.set.ASRURL = "" }, ""},
		{"no folder", func(r *searchRig) { r.ask.Folder, r.ask.Path = "", "" }, "none of the plugin's folders"},
		{"unknown folder", func(r *searchRig) { r.ask.Folder = "more_movies" }, "not approved"},
		{"file in another folder", func(r *searchRig) { r.ask.Folder = "shows" }, "is not a file in the shows folder"},
		{"escaping path", func(r *searchRig) { r.ask.Path = "../../etc/passwd" }, "refused path"},
		{"absolute path", func(r *searchRig) { r.ask.Path = "/etc/passwd" }, "refused path"},
		{"mp4", func(r *searchRig) {
			touch(t, filepath.Join(r.s.folders[0].Path, "A/A.mp4"))
			r.ask.Path = "A/A.mp4"
		}, "container is not supported"},
		{"no language wanted", func(r *searchRig) { r.ask.Languages = nil }, ""},
	}
	for _, c := range cases {
		r := newSearchRig(t, track(1, "eng", "A_AAC", 0))
		c.edit(r)
		if got := r.s.search(context.Background(), r.ask); got == nil || len(got) != 0 {
			t.Errorf("%s: offers %+v, want an empty list", c.name, got)
		}
		if c.log != "" && !strings.Contains(r.log.String(), c.log) {
			t.Errorf("%s: log %q lacks %q", c.name, r.log, c.log)
		}
	}
}

// A slow read answers in time with nothing; the next search uses what it
// read. A repeat search does not read the file again.
func TestSearchSlowRead(t *testing.T) {
	r := newSearchRig(t)
	gate := make(chan struct{})
	r.s.tracks.open = func(string) (video, io.Closer, error) {
		r.opens.Add(1)
		<-gate
		return tracksOnly{track(1, "eng", "A_AAC", 0)}, io.NopCloser(nil), nil
	}
	r.s.wait = 50 * time.Millisecond
	t0 := time.Now()
	if got := r.langs(); len(got) != 0 {
		t.Fatalf("offers %v while still reading", got)
	}
	if d := time.Since(t0); d > time.Second {
		t.Errorf("answered after %s", d)
	}
	if !strings.Contains(r.log.String(), "took over 50ms") {
		t.Errorf("log: %s", r.log)
	}
	close(gate)
	waitFor(t, "a second search with offers", func() bool { return len(r.langs()) == 1 })
	r.langs()
	if n := r.opens.Load(); n != 1 {
		t.Errorf("file read %d times", n)
	}
	if searchWait != 10*time.Second || maxOffers != 20 {
		t.Error("search waits 10 s and offers at most 20")
	}
}

// A file that could not be read is tried again on the next search.
func TestSearchReadErrorNotKept(t *testing.T) {
	r := newSearchRig(t)
	fail := true
	r.s.tracks.open = func(string) (video, io.Closer, error) {
		r.opens.Add(1)
		if fail {
			return nil, nil, errors.New("input/output error")
		}
		return tracksOnly{track(1, "eng", "A_AAC", 0)}, io.NopCloser(nil), nil
	}
	if got := r.langs(); len(got) != 0 {
		t.Fatalf("offers %v", got)
	}
	fail = false
	if got := r.langs(); len(got) != 1 {
		t.Fatalf("offers %v after the error passed", got)
	}
}

// At most 20 offers.
func TestSearchOfferCap(t *testing.T) {
	var tracks []media.Track
	var langs []string
	for i, l := range [][2]string{{"en", "eng"}, {"fr", "fre"}, {"de", "ger"}, {"es", "spa"}, {"it", "ita"}, {"nl", "dut"},
		{"pl", "pol"}, {"pt", "por"}, {"ru", "rus"}, {"sv", "swe"}, {"da", "dan"}, {"fi", "fin"}, {"no", "nor"}, {"cs", "cze"},
		{"hu", "hun"}, {"tr", "tur"}, {"el", "gre"}, {"he", "heb"}, {"ja", "jpn"}, {"ko", "kor"}, {"zh", "chi"}, {"uk", "ukr"}} {
		tracks = append(tracks, track(uint64(i+1), l[1], "A_AAC", 0))
		langs = append(langs, l[0])
	}
	r := newSearchRig(t, tracks...)
	r.ask.Languages = langs
	if got := r.langs(); len(got) != maxOffers {
		t.Errorf("%d offers", len(got))
	}
}
