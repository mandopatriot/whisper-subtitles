package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func fixtureLines(t *testing.T) [][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(b), []byte("\n"))
}

func TestReadEventFixtures(t *testing.T) {
	lines := fixtureLines(t)
	var s State
	type want struct {
		job  *Job
		skip string // part of the skip reason; "" for a job
	}
	wants := []want{
		{job: &Job{EventID: 5120, TitleID: 42, Kind: "movie", FileID: 311, Path: "Arrival (2016)/Arrival (2016).en.srt"}},
		{job: &Job{EventID: 5121, TitleID: 77, Kind: "episode", FileID: 902, Path: "Secret Invasion (2023)/Season 01/Secret Invasion (2023) - S01E01.en.srt"}},
		{skip: "subtitle_failed"},
		{skip: "subtitle_site_paused"},
		{skip: "test event"},
		{skip: "after id 4100"},
	}
	if len(lines) != len(wants) {
		t.Fatalf("%d fixture lines, want %d", len(lines), len(wants))
	}
	for i, w := range wants {
		j, err := s.readEvent(lines[i])
		if w.job != nil {
			if err != nil || j == nil || *j != *w.job {
				t.Errorf("line %d: got %+v, %v; want %+v", i, j, err, *w.job)
			}
			continue
		}
		if j != nil || !errors.Is(err, errSkip) || !strings.Contains(err.Error(), w.skip) {
			t.Errorf("line %d: got %+v, %v; want a skip naming %q", i, j, err, w.skip)
		}
	}
	if s.Highest != 5123 {
		t.Errorf("highest = %d, want 5123 (test and missed lines never move it)", s.Highest)
	}
}

func TestReadEventDuplicatesAndOrder(t *testing.T) {
	lines := fixtureLines(t)
	s := State{Highest: 5120}
	if j, err := s.readEvent(lines[0]); j != nil || !errors.Is(err, errSkip) {
		t.Errorf("id at highest: got %+v, %v; want skip", j, err)
	}
	if j, err := s.readEvent(lines[1]); j == nil || err != nil {
		t.Errorf("id above highest: got %+v, %v; want a job", j, err)
	}
	if j, err := s.readEvent(lines[1]); j != nil || !errors.Is(err, errSkip) {
		t.Errorf("same line again: got %+v, %v; want skip", j, err)
	}
	if j, err := s.readEvent(lines[0]); j != nil || !errors.Is(err, errSkip) {
		t.Errorf("older id after newer: got %+v, %v; want skip", j, err)
	}
}

func TestReadEventBadLines(t *testing.T) {
	for _, line := range []string{
		`not json`,
		`{"id":7}`,
		`{"event":"library.subtitle","payload":{"event_kind":"subtitle_saved"}}`,
		`{"event":"library.subtitle","id":9,"payload":{"event_kind":"subtitle_saved","data":{"file_id":1,"path":"a.srt"}}}`,
		`{"event":"library.subtitle","id":10,"payload":{"event_kind":"subtitle_saved","movie":{"id":1},"show":{"id":2},"data":{"file_id":1,"path":"a.srt"}}}`,
		`{"event":"library.subtitle","id":11,"payload":{"event_kind":"subtitle_saved","movie":{"id":1},"data":{"file_id":1}}}`,
		`{"event":"library.subtitle","id":12,"payload":{"event_kind":"subtitle_saved","movie":{"id":1},"data":{"file_id":1,"path":"/abs/a.srt"}}}`,
	} {
		var s State
		j, err := s.readEvent([]byte(line))
		if j != nil || err == nil || errors.Is(err, errSkip) {
			t.Errorf("%s: got %+v, %v; want a bad line error", line, j, err)
		}
	}
	var s State
	if j, err := s.readEvent([]byte("  \n")); j != nil || err != nil {
		t.Errorf("blank line: got %+v, %v", j, err)
	}
}

func TestStatePersists(t *testing.T) {
	dir := t.TempDir()
	s, err := loadState(dir)
	if err != nil || s.Highest != 0 || len(s.Pending) != 0 {
		t.Fatalf("fresh state = %+v, %v", s, err)
	}
	lines := fixtureLines(t)
	j, _ := s.readEvent(lines[0])
	s.push(*j)
	s.CheckTitlesDone = "42"
	if err := s.save(dir); err != nil {
		t.Fatal(err)
	}
	// A restart: HolyArrs replays the same line during catch-up.
	s2, err := loadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Highest != 5120 || s2.CheckTitlesDone != "42" || len(s2.Pending) != 1 || s2.Pending[0] != *j {
		t.Errorf("reloaded = %+v", s2)
	}
	if j2, err := s2.readEvent(lines[0]); j2 != nil || !errors.Is(err, errSkip) {
		t.Errorf("replayed line after restart: got %+v, %v; want skip", j2, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("data folder has %d files, want only state.json", len(entries))
	}
}

func TestStateBadFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/state.json", []byte("{broken"), 0o600)
	if _, err := loadState(dir); err == nil {
		t.Error("a broken state.json must be reported, not silently reset")
	}
}

func TestStateQueue(t *testing.T) {
	var s State
	a := Job{EventID: 1, TitleID: 1, FileID: 1, Path: "a.srt"}
	if d := s.push(a); d != nil || len(s.Pending) != 1 {
		t.Fatal("first push")
	}
	if d := s.push(a); d != nil || len(s.Pending) != 1 {
		t.Error("the same job twice must stay one job")
	}
	for i := 2; i <= maxPending; i++ {
		s.push(Job{EventID: int64(i), TitleID: 1})
	}
	d := s.push(Job{EventID: maxPending + 1, TitleID: 1})
	if d == nil || *d != a || len(s.Pending) != maxPending {
		t.Errorf("over %d jobs: dropped %+v, %d waiting; want the oldest dropped", maxPending, d, len(s.Pending))
	}
	s.done(Job{EventID: 2, TitleID: 1})
	if len(s.Pending) != maxPending-1 || s.Pending[0].EventID != 3 {
		t.Errorf("done: %d waiting, first %+v", len(s.Pending), s.Pending[0])
	}
}
