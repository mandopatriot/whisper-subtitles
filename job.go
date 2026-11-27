package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"whisper-subtitles/media"
)

// Limits for the audio check (plan sections 3 and 5).
const (
	uploadCapBytes = 64 << 20 // one window's upload
	minWindowMs    = 30_000   // a window shorter than this is not sent
	backoffFirst   = 5 * time.Minute
	backoffMax     = 60 * time.Minute
)

// videoExts are the containers read today. MP4 and others are skipped with
// a log line until they are built.
var videoExts = map[string]bool{".mkv": true, ".webm": true, ".mk3d": true}

// video is what the check reads from a video file.
type video interface {
	Duration() int64 // ms
	AllTracks() []media.Track
	Extract(t media.Track, startMs, durMs, maxBytes int64, w io.Writer) (media.Window, error)
}

type mkvVideo struct{ *media.MKV }

func (v mkvVideo) Duration() int64          { return v.DurationMs }
func (v mkvVideo) AllTracks() []media.Track { return v.Tracks }

// openVideo opens a Matroska or WebM file for reading as a stream.
func openVideo(p string) (video, io.Closer, error) {
	if !videoExts[strings.ToLower(filepath.Ext(p))] {
		return nil, nil, fmt.Errorf("%w: the %s container is not supported yet (MKV and WebM are)", errRule, filepath.Ext(p))
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	m, err := media.OpenMKV(f, fi.Size())
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("%w: can't read %s: %v", errRule, filepath.Base(p), err)
	}
	return mkvVideo{m}, f, nil
}

// transcriber is the Whisper client as the check uses it.
type transcriber interface {
	Transcribe(ctx context.Context, path, lang string) (Transcript, error)
}

// checker checks one subtitle against the audio and syncs it when sure.
type checker struct {
	ha        *HolyArrs
	asr       transcriber
	queue     *asrQueue
	dataDir   string
	checkOnly bool
	logf      func(format string, args ...any)
	open      func(path string) (video, io.Closer, error)
}

// readSub reads a subtitle file (at most maxSubBytes) and its SHA-256.
func readSub(p string) ([]byte, [32]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSubBytes+1))
	if err != nil {
		return nil, [32]byte{}, err
	}
	if len(b) > maxSubBytes {
		return nil, [32]byte{}, fmt.Errorf("%w: the subtitle is over %d bytes", errRule, maxSubBytes)
	}
	return b, sha256.Sum256(b), nil
}

// check is the app's checkFunc.
func (k *checker) check(ctx context.Context, c Candidate, loc Located) error {
	b, sum, err := readSub(loc.Sub)
	if err != nil {
		return err
	}
	cues, err := readCues(b, c.Format)
	if err != nil {
		return fmt.Errorf("%w: %v", errRule, err)
	}
	v, closer, err := k.open(loc.Video)
	if err != nil {
		return err
	}
	defer closer.Close()
	ch, err := media.PickAudio(v.AllTracks(), c.Language)
	if err != nil {
		// No track in the subtitle's language: nothing is uploaded.
		return fmt.Errorf("%w: %v", errRule, err)
	}
	lang := ""
	if !ch.Untagged {
		lang = media.BaseLanguage(ch.Track.Language)
	}
	wins, err := k.listen(ctx, c.String(), v, ch.Track, lang)
	if err != nil {
		return err
	}
	r := fitSubtitle(cues, c.Language, wins)
	if !r.Sync {
		// Too few windows, when lost windows could be why: the language rule
		// counts only when the lost ones could have made up the shortfall.
		lost, first := lostWindows(wins)
		langShort := r.LangWindows < fitMinLangWindows && r.LangWindows+lost >= fitMinLangWindows
		if lost > 0 && (r.TooFew || langShort) {
			return fmt.Errorf("%w: %d of %d windows were lost to the connection (%v); %s", errCutOff, lost, len(wins), first, r)
		}
		return fmt.Errorf("%w: %s", errRule, r)
	}
	fix := SyncFix{OffsetMS: r.OffsetMs, FromFPS: r.FromFPS, ToFPS: r.ToFPS}
	if err := k.unchanged(ctx, c, loc, sum); err != nil {
		return err
	}
	if k.checkOnly {
		k.logf("%s: check only, would send %s: %s", c, fix, r)
		return nil
	}
	nr, err := k.ha.Sync(ctx, c.SubtitleID, fix)
	if err != nil {
		return fmt.Errorf("sync %s: %w", fix, err)
	}
	k.logf("%s: synced %s, now subtitle %d: %s", c, fix, nr.ID, r)
	return nil
}

// errCutOff is a check that could not finish because windows were lost to
// the connection to Whisper: it is tried again later, not left alone.
var errCutOff = errors.New("cut off")

// lostWindows counts the windows lost to the connection to Whisper (dropped,
// away, busy or refused by the proxy), with the first such error.
func lostWindows(wins []WindowText) (n int, first error) {
	for _, w := range wins {
		e := w.Err
		if errors.Is(e, errASRLost) || errors.Is(e, errASRDown) || errors.Is(e, errASRTimeout) || errors.Is(e, errASRForbidden) {
			if n == 0 {
				first = e
			}
			n++
		}
	}
	return n, first
}

// listen sends the windows of track t to Whisper and returns what it heard
// in each. A window too short, empty or failed counts as failed; an upload
// that got no answer in time ends the check, since the server may be stuck.
func (k *checker) listen(ctx context.Context, who string, v video, t media.Track, lang string) ([]WindowText, error) {
	durMs := int64(media.WindowMs)
	if t.BPS > 0 {
		durMs = min(durMs, uploadCapBytes*8*1000/t.BPS)
	}
	if durMs < minWindowMs {
		return nil, fmt.Errorf("%w: the %s audio track runs at %d kb/s, so a %d MiB upload holds under %d s",
			errRule, t.CodecID, t.BPS/1000, uploadCapBytes>>20, minWindowMs/1000)
	}
	starts := media.WindowStarts(v.Duration())
	if len(starts) == 0 {
		return nil, fmt.Errorf("%w: the video's duration is unknown", errRule)
	}
	wins := make([]WindowText, 0, len(starts))
	for i, st := range starts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w := k.window(ctx, fmt.Sprintf("%s, window %d of %d", who, i+1, len(starts)), v, t, st, durMs, lang)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if errors.Is(w.Err, errASRTimeout) {
			return nil, w.Err
		}
		wins = append(wins, w)
	}
	return wins, nil
}

// window cuts one window to a file in the data folder, uploads it and
// removes the file. who names it in the log when it waits for the slot.
func (k *checker) window(ctx context.Context, who string, v video, t media.Track, startMs, durMs int64, lang string) WindowText {
	f, err := newWindowFile(k.dataDir)
	if err != nil {
		return WindowText{BaseMs: startMs, Err: err}
	}
	defer os.Remove(f.Name())
	win, err := v.Extract(t, startMs, durMs, uploadCapBytes, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return WindowText{BaseMs: startMs, Err: err}
	case win.Blocks == 0:
		return WindowText{BaseMs: startMs, Err: fmt.Errorf("no audio at %d ms", startMs)}
	case win.Capped && win.DurationMs() < minWindowMs:
		return WindowText{BaseMs: win.BaseMs, Err: fmt.Errorf("only %d ms fit in %d MiB", win.DurationMs(), uploadCapBytes>>20)}
	}
	var tr Transcript
	var waitFrom time.Time
	err = k.queue.send(ctx, prioSync, func(holder asrPrio, ahead int) {
		waitFrom = time.Now()
		goes := fmt.Sprintf("%d uploads go", ahead)
		if ahead == 1 {
			goes = "1 upload goes"
		}
		k.logf("%s: waiting for the Whisper slot, held by %s (%s first)", who, holder, goes)
	}, nil, func() error {
		if !waitFrom.IsZero() {
			k.logf("%s: got the Whisper slot after %s", who, time.Since(waitFrom).Round(100*time.Millisecond))
			waitFrom = time.Time{}
		}
		var err error
		tr, err = k.asr.Transcribe(withUploadName(ctx, who), f.Name(), lang)
		return err
	})
	switch {
	case errors.Is(err, errASRTimeout):
		k.logf("Whisper gave no answer in time; no uploads for %s: %v", k.queue.timedOut(), err)
	case err == nil:
		k.queue.answered()
	}
	return WindowText{BaseMs: win.BaseMs, Tr: tr, Err: err}
}

// unchanged checks, just before the sync, that the subtitle is still the one
// that was fitted: the same present row, never synced, and the same bytes.
func (k *checker) unchanged(ctx context.Context, c Candidate, loc Located, sum [32]byte) error {
	files, err := k.ha.TitleSubtitles(ctx, c.TitleID)
	if err != nil {
		return err
	}
	var row *SubRow
	for _, f := range files {
		for i, r := range f.Subtitles {
			if r.ID == c.SubtitleID && f.FileID == c.FileID {
				row = &f.Subtitles[i]
			}
		}
	}
	switch {
	case row == nil:
		return fmt.Errorf("%w: the subtitle is gone from HolyArrs since it was read", errRule)
	case row.State != "present" || row.RelativePath == nil || *row.RelativePath != c.SubRel:
		return fmt.Errorf("%w: the subtitle changed in HolyArrs since it was read (now %s)", errRule, row.State)
	case len(row.Sync) > 0:
		return fmt.Errorf("%w: its timing was changed since it was read", errRule)
	}
	_, now, err := readSub(loc.Sub)
	if err != nil {
		return err
	}
	if now != sum {
		return fmt.Errorf("%w: the file changed since it was read", errRule)
	}
	return nil
}
