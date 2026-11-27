package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"whisper-subtitles/media"
)

// How a whole film reaches Whisper: in chunks of chunkMs, each read with
// chunkPadMs more on both sides so speech across a cut is heard whole. A
// chunk owns the speech that starts inside it; the pads only help Whisper.
const (
	chunkMs    = 90_000
	chunkPadMs = 15_000
	// echoSlackMs: a segment at the start of a chunk that starts this much
	// before the end of the last kept one is the previous chunk's speech
	// heard again, and is dropped.
	echoSlackMs = 200
	// Whisper makes up a lone thanks or "you" where audio starts: at an
	// upload's start and at each engineStepMs the engine decodes in. Such a
	// line starting within edgeSlackMs of one is dropped.
	engineStepMs = 30_000
	edgeSlackMs  = 250
)

// edgeFillers are the whole lines, lowercased and without punctuation, that
// are dropped at a piece edge. Anywhere else they are kept: people say them.
var edgeFillers = map[string]bool{"you": true, "thank you": true, "thank": true, "thanks for watching": true, "thank you for watching": true}

// fillerLine says whether text is one of edgeFillers.
func fillerLine(text string) bool {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	return edgeFillers[strings.Join(words, " ")]
}

// atEdge says whether relMs, a time inside an upload, is within
// edgeSlackMs of the upload's start or of a engineStepMs mark in it.
func atEdge(relMs int64) bool {
	if relMs < 0 {
		return false
	}
	m := relMs % engineStepMs
	return m <= edgeSlackMs || engineStepMs-m <= edgeSlackMs
}

// offerKey is what an offer's key names: the track, its language, and the
// video's size and time, so an upgraded video never gets an old subtitle.
type offerKey struct {
	Track uint64
	Lang  string // two-letter base, as sent to Whisper
	Size  int64
	MTime int64 // Unix seconds
}

func (k offerKey) String() string {
	return fmt.Sprintf("v1:t%d:%s:%d:%d", k.Track, k.Lang, k.Size, k.MTime)
}

var keyRe = regexp.MustCompile(`^v1:t([0-9]{1,9}):([a-z]{2,3}):([0-9]{1,19}):(-?[0-9]{1,19})$`)

func parseKey(s string) (offerKey, error) {
	m := keyRe.FindStringSubmatch(s)
	if m == nil {
		return offerKey{}, fmt.Errorf("%q is not one of this plugin's keys", s)
	}
	var k offerKey
	var err error
	if k.Track, err = strconv.ParseUint(m[1], 10, 64); err != nil {
		return offerKey{}, err
	}
	k.Lang = m[2]
	if k.Size, err = strconv.ParseInt(m[3], 10, 64); err != nil {
		return offerKey{}, err
	}
	if k.MTime, err = strconv.ParseInt(m[4], 10, 64); err != nil {
		return offerKey{}, err
	}
	return k, nil
}

// resolveVideo finds a video by the folder name and path a hook job sends.
func resolveVideo(folders []Folder, folder, rel string) (string, os.FileInfo, error) {
	if folder == "" || rel == "" {
		return "", nil, errors.New("the video is in none of the plugin's folders")
	}
	if !safeRel(rel) {
		return "", nil, fmt.Errorf("refused path %q", rel)
	}
	for _, f := range folders {
		if f.Name != folder {
			continue
		}
		full, ok := inside(f.Path, rel)
		if !ok {
			return "", nil, fmt.Errorf("%s is not a file in the %s folder", rel, folder)
		}
		fi, err := os.Stat(full)
		if err != nil {
			return "", nil, err
		}
		return full, fi, nil
	}
	return "", nil, fmt.Errorf("the %s folder is not approved for this plugin", folder)
}

// makeReq is one make, as the hook sends it.
type makeReq struct {
	Key      string `json:"key"`
	Language string `json:"language"`
	FileID   int64  `json:"file_id"`
	Folder   string `json:"folder"`
	Path     string `json:"path"`
}

// chunkTranscriber is the Whisper client as a make uses it.
type chunkTranscriber interface {
	TranscribeChunk(ctx context.Context, path, lang string) (Transcript, error)
}

// maker writes a subtitle from a video's audio.
type maker struct {
	dataDir string
	folders []Folder
	open    func(path string) (video, io.Closer, error)
	asr     chunkTranscriber
	queue   *asrQueue
	cache   *madeCache
	logf    func(format string, args ...any)
	// pieceMs is the audio each upload owns; 0 means chunkMs.
	pieceMs int64
}

func (m *maker) piece() int64 {
	if m.pieceMs > 0 {
		return m.pieceMs
	}
	return chunkMs
}

// errMakeRule is a make refused before anything is sent to Whisper.
var errMakeRule = errors.New("can't write this subtitle")

// cached answers a make from the cache, when it was finished before for
// the video as it is now.
func (m *maker) cached(r makeReq) ([]byte, bool) {
	if _, _, err := m.current(r); err != nil {
		return nil, false
	}
	return m.cache.get(madeID(r.FileID, r.Key), r.FileID, r.Key)
}

// current checks r against the video as it is now: the key is one of ours,
// in the language asked, for a video of the same size and time.
func (m *maker) current(r makeReq) (string, offerKey, error) {
	rule := func(f string, a ...any) error { return fmt.Errorf("%w: %s", errMakeRule, fmt.Sprintf(f, a...)) }
	k, err := parseKey(r.Key)
	if err != nil {
		return "", k, rule("%v", err)
	}
	if media.BaseLanguage(r.Language) != k.Lang {
		return "", k, rule("asked for %q, but the offer was for %s", r.Language, k.Lang)
	}
	full, fi, err := resolveVideo(m.folders, r.Folder, r.Path)
	if err != nil {
		return "", k, rule("%v", err)
	}
	if fi.Size() != k.Size || fi.ModTime().Unix() != k.MTime {
		return "", k, rule("the video changed since the search; search again")
	}
	return full, k, nil
}

// make writes the subtitle r asks for, resuming from finished chunks.
func (m *maker) make(ctx context.Context, r makeReq) ([]byte, error) {
	rule := func(f string, a ...any) error { return fmt.Errorf("%w: %s", errMakeRule, fmt.Sprintf(f, a...)) }
	full, k, err := m.current(r)
	if err != nil {
		return nil, err
	}
	id := madeID(r.FileID, r.Key)
	if b, ok := m.cache.get(id, r.FileID, r.Key); ok {
		return b, nil
	}
	v, closer, err := m.open(full)
	if err != nil {
		return nil, rule("%v", err)
	}
	defer closer.Close()
	var t media.Track
	found := false
	for _, tr := range v.AllTracks() {
		if tr.Number == k.Track {
			t, found = tr, true
		}
	}
	switch {
	case !found:
		return nil, rule("the video has no track %d any more", k.Track)
	case !t.Dialogue():
		return nil, rule("track %d is not a dialogue audio track", k.Track)
	case media.BaseLanguage(t.Language) != k.Lang:
		return nil, rule("track %d is %q, not %s", k.Track, t.Language, k.Lang)
	}
	dur := v.Duration()
	if dur <= 0 {
		return nil, rule("the video's duration is unknown")
	}
	if err := m.cache.openWork(id, r.FileID, r.Key); err != nil {
		return nil, err
	}
	name := filepath.Base(r.Path)
	var segs []tSeg
	var dropped, fillers int
	start, i, reused := int64(0), 0, 0
	for start < dur {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ch, ok := m.cache.chunk(id, i)
		if ok && ch.Start == start {
			reused++
		} else {
			if reused > 0 && reused == i {
				m.logf("%s, %s: resuming at chunk %d of about %d (%s); %d chunks reused from before",
					name, k.Lang, i+1, (dur+m.piece()-1)/m.piece(), srtStamp(start), reused)
			}
			ch, err = m.chunk(ctx, v, t, k.Lang, start, dur, segs)
			if err != nil {
				return nil, fmt.Errorf("%s, %s, chunk %d at %s: %w", name, k.Lang, i+1, srtStamp(start), err)
			}
			if err := m.cache.putChunk(id, i, ch); err != nil {
				return nil, err
			}
			for _, n := range ch.Notes {
				m.logf("%s, %s, chunk %d: %s", name, k.Lang, i+1, n)
			}
		}
		dropped += countNote(ch.Notes, droppedNote)
		fillers += countNote(ch.Notes, fillerNote)
		segs = append(segs, ch.Segs...)
		start, i = ch.Next, i+1
	}
	if reused > 0 && reused == i {
		m.logf("%s, %s: all %d chunks reused from before", name, k.Lang, reused)
	}
	body, n, err := buildSRT(segs, dur)
	if err != nil {
		if errors.Is(err, errNoSpeech) {
			return nil, fmt.Errorf("%w in %s (%d lines)", err, k.Lang, n)
		}
		return nil, err
	}
	if err := m.cache.put(id, r.FileID, r.Key, body); err != nil {
		m.logf("could not keep the finished subtitle: %v", err)
	}
	m.logf("%s, %s: wrote %d lines in %d chunks; %d segments dropped as not speech, %d as filler at a piece edge", name, k.Lang, n, i, dropped, fillers)
	return body, nil
}

const (
	droppedNote = "segments dropped as not speech: "
	fillerNote  = "segments dropped as filler at a piece edge: "
)

func countNote(notes []string, prefix string) int {
	n := 0
	for _, s := range notes {
		if v, ok := strings.CutPrefix(s, prefix); ok {
			c, _ := strconv.Atoi(v)
			n += c
		}
	}
	return n
}

// chunk cuts, sends and stitches the chunk owning speech from start. prev is
// the speech kept so far, for the echo rule.
func (m *maker) chunk(ctx context.Context, v video, t media.Track, lang string, start, dur int64, prev []tSeg) (chunkFile, error) {
	from := max(0, start-chunkPadMs)
	f, err := newWindowFile(m.dataDir)
	if err != nil {
		return chunkFile{}, err
	}
	defer os.Remove(f.Name())
	win, err := v.Extract(t, from, start+m.piece()+chunkPadMs-from, uploadCapBytes, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return chunkFile{}, err
	}
	ownEnd := start + m.piece()
	if win.Blocks == 0 {
		// No audio here (a gap in the track): nothing to hear.
		return chunkFile{Start: start, Next: min(ownEnd, dur), Notes: []string{"no audio"}}, nil
	}
	if win.Capped {
		ownEnd = win.EndMs - chunkPadMs
		if ownEnd-start < minWindowMs {
			return chunkFile{}, fmt.Errorf("%w: the %s audio track is too large per second to send (%d MiB held %d s)",
				errMakeRule, t.CodecID, uploadCapBytes>>20, max(0, win.DurationMs())/1000)
		}
	}
	last := ownEnd >= dur
	if last {
		ownEnd = math.MaxInt64 // the last chunk keeps everything after start
	}
	release, err := m.queue.acquire(ctx, prioMake)
	if err != nil {
		return chunkFile{}, err
	}
	tr, err := m.asr.TranscribeChunk(ctx, f.Name(), lang)
	release()
	switch {
	case errors.Is(err, errASRTimeout):
		m.logf("Whisper gave no answer in time; no uploads for %s: %v", m.queue.timedOut(), err)
		return chunkFile{}, err
	case err != nil:
		return chunkFile{}, err
	}
	m.queue.answered()
	lastEnd := int64(-1) // nothing kept yet
	for _, s := range prev {
		lastEnd = max(lastEnd, s.EndMs)
	}
	kept, dropped, fillers := ownSegments(tr, win.BaseMs, start, ownEnd, lastEnd)
	next := ownEnd
	if last {
		next = dur
	}
	ch := chunkFile{Start: start, Next: next, Segs: kept}
	if dropped > 0 {
		ch.Notes = append(ch.Notes, droppedNote+strconv.Itoa(dropped))
	}
	if len(fillers) > 0 {
		ch.Notes = append(ch.Notes, fillers...)
		ch.Notes = append(ch.Notes, fillerNote+strconv.Itoa(len(fillers)))
	}
	if tr.Overruns > 0 {
		ch.Notes = append(ch.Notes, fmt.Sprintf("%d segments ran past the audio and were dropped", tr.Overruns))
	}
	return ch, nil
}

// ownSegments keeps the speech of one chunk: segments Whisper did not mark
// as not speech, with text, starting in [start, ownEnd), and not an echo of
// the previous chunk's last line (lastEnd, -1 when nothing was kept yet).
// Times move from the upload's to the video's by baseMs. dropped counts the
// not-speech segments; fillers describes each filler line dropped at a piece
// edge.
func ownSegments(tr Transcript, baseMs, start, ownEnd, lastEnd int64) (kept []tSeg, dropped int, fillers []string) {
	at := func(sec float64) int64 { return baseMs + int64(math.Round(sec*1000)) }
	segs := append([]Segment(nil), tr.Segments...)
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].Start < segs[j].Start })
	for _, s := range segs {
		st, en := at(s.Start), at(s.End)
		if st < start || st >= ownEnd {
			continue // another chunk's
		}
		if notSpeech(s) {
			dropped++
			continue
		}
		text := cleanText(s.Text)
		if text == "" {
			continue
		}
		if rel := int64(math.Round(s.Start * 1000)); fillerLine(text) && atEdge(rel) {
			fillers = append(fillers, fmt.Sprintf("dropped %q at %s, %.1f s into the upload", text, srtStamp(st), float64(rel)/1000))
			continue
		}
		if lastEnd >= 0 && st < start+chunkPadMs && st < lastEnd-echoSlackMs {
			continue // heard again across the cut
		}
		seg := tSeg{StartMs: st, EndMs: en, Text: text}
		for _, w := range s.Words {
			seg.Words = append(seg.Words, tWord{StartMs: at(w.Start), EndMs: at(w.End), Text: w.Word})
		}
		kept = append(kept, seg)
		lastEnd = max(lastEnd, en)
	}
	return kept, dropped, fillers
}
