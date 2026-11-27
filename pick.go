package main

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Candidate is a subtitle that passed every rule and may be checked against
// the audio.
type Candidate struct {
	TitleID    int64 // set by the job
	SubtitleID int64
	FileID     int64
	VideoRel   string // video path inside the Library folder
	SubRel     string // subtitle path inside the Library folder
	Language   string // HolyArrs code, such as en or pt-BR
	Format     string // srt, vtt, ass or ssa
	Score      *int
}

func (c Candidate) String() string {
	return fmt.Sprintf("subtitle %d (%s, %s)", c.SubtitleID, c.Language, c.SubRel)
}

// madeTranscribed is SubRow.Made for a subtitle written from the audio.
const madeTranscribed = "transcribed"

// retimeFormats are the formats HolyArrs's Sync can change (subtext.Retime).
var retimeFormats = map[string]bool{"srt": true, "vtt": true, "ass": true, "ssa": true}

// errRule marks a subtitle a rule leaves alone. The text names the rule.
var errRule = errors.New("left alone")

// checkRow applies the rules of plan (2)-5 to one row. kind is movie or
// episode; manual runs (check_titles) skip only the score rule.
func checkRow(r SubRow, kind string, set Settings, manual bool) error {
	rule := func(f string, a ...any) error { return fmt.Errorf("%w: %s", errRule, fmt.Sprintf(f, a...)) }
	switch {
	case r.Place != "beside":
		return rule("it is not a file beside the video (%s)", r.Place)
	case r.State != "present":
		return rule("it is %s, not present", r.State)
	case r.RelativePath == nil || *r.RelativePath == "":
		return rule("it has no path")
	case r.Origin != "fetched" && r.Origin != "by_hand":
		// HolyArrs's Sync accepts any beside file, but a file HolyArrs did not
		// save is the owner's; it is never changed here.
		return rule("HolyArrs did not save it (origin %s)", r.Origin)
	case len(r.Sync) > 0:
		return rule("its timing was already changed once")
	case r.Made != nil && *r.Made == madeTranscribed:
		// Written from this video's own audio: its timing already is the
		// audio's, and checking it would only keep the server busy.
		return rule("it was written from the audio")
	}
	if f := format(*r.RelativePath); !retimeFormats[f] {
		return rule("HolyArrs can't change the timing of .%s files", f)
	}
	if manual {
		return nil
	}
	limit := set.ThresholdMovies
	if kind == "episode" {
		limit = set.ThresholdEpisodes
	}
	if r.Score != nil && *r.Score >= limit {
		return rule("its score %d is at or above the %s threshold %d", *r.Score, kind, limit)
	}
	return nil
}

func format(p string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(p), "."))
}

func candidate(f FileView, r SubRow) Candidate {
	return Candidate{SubtitleID: r.ID, FileID: f.FileID, VideoRel: f.RelativePath, SubRel: *r.RelativePath,
		Language: r.Language, Format: format(*r.RelativePath), Score: r.Score}
}

// pickForEvent finds the subtitle an event names and checks it.
func pickForEvent(files []FileView, j Job, set Settings) (Candidate, error) {
	for _, f := range files {
		if f.FileID != j.FileID {
			continue
		}
		if f.RelativePath == "" {
			return Candidate{}, fmt.Errorf("%w: file %d has no video path", errRule, f.FileID)
		}
		for _, r := range f.Subtitles {
			if r.RelativePath == nil || *r.RelativePath != j.Path || r.State != "present" {
				continue
			}
			if err := checkRow(r, j.Kind, set, false); err != nil {
				return Candidate{}, err
			}
			return candidate(f, r), nil
		}
		return Candidate{}, fmt.Errorf("%w: no present subtitle %s on file %d any more", errRule, j.Path, j.FileID)
	}
	return Candidate{}, fmt.Errorf("%w: title %d has no file %d any more", errRule, j.TitleID, j.FileID)
}

// pickForTitle lists every subtitle of a title that passes the rules, for a
// check_titles run. skipped has one line for each beside subtitle left alone;
// rows that are not files (wishes, tracks inside the video) are not listed.
func pickForTitle(files []FileView, set Settings) (picked []Candidate, skipped []string) {
	for _, f := range files {
		for _, r := range f.Subtitles {
			if r.Place != "beside" || r.State != "present" {
				continue
			}
			if f.RelativePath == "" {
				skipped = append(skipped, fmt.Sprintf("subtitle %d: file %d has no video path", r.ID, f.FileID))
				continue
			}
			if err := checkRow(r, "", set, true); err != nil {
				skipped = append(skipped, fmt.Sprintf("subtitle %d: %v", r.ID, err))
				continue
			}
			picked = append(picked, candidate(f, r))
		}
	}
	return picked, skipped
}
