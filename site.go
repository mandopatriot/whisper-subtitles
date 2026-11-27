package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"whisper-subtitles/media"
)

const (
	// searchWait is how long a search looks before answering. HolyArrs
	// waits 30 s, and asks every site in turn.
	searchWait = 10 * time.Second
	maxOffers  = 20
	// trackCacheMax is how many videos' track lists are kept in memory.
	trackCacheMax = 500
)

// searchAsk is one search, as the hook sends it. Only what the offers need
// is read.
type searchAsk struct {
	Kind      string   `json:"kind"`
	Title     string   `json:"title"`
	Languages []string `json:"languages"`
	FileID    int64    `json:"file_id"`
	Folder    string   `json:"folder"`
	Path      string   `json:"path"`
}

// offer is one subtitle the plugin can write.
type offer struct {
	Key             string `json:"key"`
	Language        string `json:"language"`
	HearingImpaired bool   `json:"hearing_impaired"`
	Forced          bool   `json:"forced"`
	Format          string `json:"format"`
}

// trackList is a video's tracks, read once.
type trackList struct {
	done   chan struct{}
	tracks []media.Track
	err    error
}

// trackCache keeps the track lists of the videos searched last, by folder,
// path, size and time, so a repeat search needs only a stat.
type trackCache struct {
	mu    sync.Mutex
	m     map[string]*trackList
	order []string
	open  func(path string) (video, io.Closer, error)
}

func newTrackCache(open func(string) (video, io.Closer, error)) *trackCache {
	return &trackCache{m: map[string]*trackList{}, open: open}
}

// get returns the entry for key, reading full in the background the first
// time. Wait on done before reading it.
func (c *trackCache) get(key, full string) *trackList {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		return e
	}
	e := &trackList{done: make(chan struct{})}
	c.m[key] = e
	c.order = append(c.order, key)
	if len(c.order) > trackCacheMax {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
	go func() {
		defer close(e.done)
		v, closer, err := c.open(full)
		if err != nil {
			e.err = err
			// Not kept: the next search tries again, as a read error may
			// pass.
			c.mu.Lock()
			if c.m[key] == e {
				delete(c.m, key)
			}
			c.mu.Unlock()
			return
		}
		e.tracks = v.AllTracks()
		closer.Close()
	}()
	return e
}

// searcher answers searches. It reads only the video's header and never
// calls Whisper.
type searcher struct {
	set     Settings
	folders []Folder
	tracks  *trackCache
	wait    time.Duration
	logf    func(format string, args ...any)
}

// search lists what the plugin can write for ask: one offer per wanted
// language spoken in a tagged dialogue track.
func (s *searcher) search(ctx context.Context, ask searchAsk) []offer {
	out := []offer{}
	if !s.set.Transcribe || s.set.ASRURL == "" {
		return out
	}
	what := fmt.Sprintf("search for file %d", ask.FileID)
	full, fi, err := resolveVideo(s.folders, ask.Folder, ask.Path)
	if err != nil {
		s.logf("%s: no offer: %v", what, err)
		return out
	}
	if ext := strings.ToLower(filepath.Ext(full)); !videoExts[ext] {
		s.logf("%s: no offer: the %s container is not supported yet (MKV and WebM are)", what, ext)
		return out
	}
	e := s.tracks.get(fmt.Sprintf("%s\x00%s\x00%d\x00%d", ask.Folder, ask.Path, fi.Size(), fi.ModTime().UnixNano()), full)
	t := time.NewTimer(s.wait)
	defer t.Stop()
	select {
	case <-e.done:
	case <-t.C:
		s.logf("%s: no offer this time: reading %s took over %s; the next search uses what it read", what, ask.Path, s.wait)
		return out
	case <-ctx.Done():
		return out
	}
	if e.err != nil {
		s.logf("%s: no offer: %v", what, e.err)
		return out
	}
	seen := map[string]bool{}
	for _, lang := range ask.Languages {
		if len(out) == maxOffers {
			break
		}
		base := media.BaseLanguage(lang)
		if base == "" || seen[lang] || !validLang(base) {
			continue
		}
		seen[lang] = true
		ch, err := media.PickAudio(e.tracks, lang)
		switch {
		case err != nil:
			continue // no track in this language
		case ch.Untagged:
			// Whisper would be told a language nobody checked.
			s.logf("%s: no %s offer: the only dialogue track has no language tag", what, lang)
			continue
		case ch.Track.BPS > 0 && uploadCapBytes*8*1000/ch.Track.BPS < minWindowMs:
			s.logf("%s: no %s offer: the %s track runs at %d kb/s, too large to send", what, lang, ch.Track.CodecID, ch.Track.BPS/1000)
			continue
		}
		k := offerKey{Track: ch.Track.Number, Lang: base, Size: fi.Size(), MTime: fi.ModTime().Unix()}
		out = append(out, offer{Key: k.String(), Language: lang, Format: "srt"})
	}
	return out
}
