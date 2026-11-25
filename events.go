package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// eventLine is the part of a HolyArrs event line this plugin reads. The
// payload comes from alerts.PluginPayload; for library.subtitle its data is
// set in subtitles.afterTarget: file_id, language, path, score, site.
type eventLine struct {
	Event   string `json:"event"`
	ID      int64  `json:"id"`
	Payload struct {
		Kind  string    `json:"event_kind"`
		Test  bool      `json:"test"`
		After int64     `json:"after"` // the "missed" line
		Movie *titleRef `json:"movie"`
		Show  *titleRef `json:"show"`
		Data  struct {
			FileID   int64  `json:"file_id"`
			Language string `json:"language"`
			Path     string `json:"path"`
		} `json:"data"`
	} `json:"payload"`
}

type titleRef struct {
	ID int64 `json:"id"`
}

// Event kinds that carry a freshly saved subtitle. library.subtitle also
// carries subtitle_failed and subtitle_site_paused, which are skipped.
const (
	kindSaved    = "subtitle_saved"
	kindUpgraded = "subtitle_upgraded"
)

// errSkip marks a line that was read fine and needs no work.
var errSkip = errors.New("skip")

// readEvent turns one line into a Job. It raises s.Highest for every real
// event it accepts or skips on purpose, so the line is not handled again
// after a restart. A nil Job with a nil error is a blank line.
func (s *State) readEvent(raw []byte) (*Job, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var ev eventLine
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, fmt.Errorf("bad line: %w", err)
	}
	switch {
	case ev.Event == "":
		return nil, errors.New("bad line: no event")
	case ev.Event == "missed":
		return nil, fmt.Errorf("%w: HolyArrs skipped events after id %d; list the titles in check_titles to check them", errSkip, ev.Payload.After)
	case ev.Payload.Test:
		return nil, fmt.Errorf("%w: test event %s received", errSkip, ev.Event)
	case ev.ID <= 0:
		return nil, fmt.Errorf("bad line: %s has no id", ev.Event)
	case ev.ID <= s.Highest:
		return nil, fmt.Errorf("%w: event %d already handled (highest %d)", errSkip, ev.ID, s.Highest)
	}
	s.Highest = ev.ID
	p := ev.Payload
	if ev.Event != "library.subtitle" {
		return nil, fmt.Errorf("%w: event %d is %s, not library.subtitle", errSkip, ev.ID, ev.Event)
	}
	if p.Kind != kindSaved && p.Kind != kindUpgraded {
		return nil, fmt.Errorf("%w: event %d is %s", errSkip, ev.ID, p.Kind)
	}
	j := Job{EventID: ev.ID, FileID: p.Data.FileID, Path: p.Data.Path}
	switch {
	case p.Movie != nil && p.Show == nil:
		j.TitleID, j.Kind = p.Movie.ID, "movie"
	case p.Show != nil && p.Movie == nil:
		j.TitleID, j.Kind = p.Show.ID, "episode"
	default:
		return nil, fmt.Errorf("bad line: event %d names no single movie or show", ev.ID)
	}
	if j.TitleID <= 0 || j.FileID <= 0 || j.Path == "" || strings.HasPrefix(j.Path, "/") {
		return nil, fmt.Errorf("bad line: event %d lacks a title id, file id or Library path", ev.ID)
	}
	return &j, nil
}
