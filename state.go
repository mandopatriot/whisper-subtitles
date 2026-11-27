package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// maxPending is how many jobs may wait; the oldest is dropped past it.
const maxPending = 500

// Job is one piece of work: a subtitle from an event, or every subtitle of a
// title for a check_titles run (FileID 0).
type Job struct {
	EventID int64  `json:"event_id,omitempty"`
	TitleID int64  `json:"title_id"`
	Kind    string `json:"kind,omitempty"` // movie or episode; "" for a check_titles run
	FileID  int64  `json:"file_id,omitempty"`
	Path    string `json:"path,omitempty"` // subtitle path inside the Library folder
	Manual  bool   `json:"manual,omitempty"`
}

func (j Job) String() string {
	if j.Manual {
		return fmt.Sprintf("title %d (check_titles)", j.TitleID)
	}
	return fmt.Sprintf("event %d, title %d, file %d, %s", j.EventID, j.TitleID, j.FileID, j.Path)
}

// State is what the plugin keeps in its data folder between starts.
type State struct {
	// Highest is the highest event id handled. Lines at or below it are skipped.
	Highest int64 `json:"highest"`
	// CheckTitlesDone is the check_titles text that was already queued.
	CheckTitlesDone string `json:"check_titles_done,omitempty"`
	// Pending are queued jobs not finished yet, oldest first.
	Pending []Job `json:"pending,omitempty"`
}

const stateFile = "state.json"

// loadState reads dir/state.json. A missing file is a fresh State.
func loadState(dir string) (State, error) {
	var s State
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%s: %w", stateFile, err)
	}
	return s, nil
}

// save writes dir/state.json through a temporary file and a rename, so a
// stop halfway leaves the old file whole.
func (s State) save(dir string) error {
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, stateFile, b)
}

// push queues j. It returns the job dropped to make room, if any.
func (s *State) push(j Job) (dropped *Job) {
	for _, p := range s.Pending {
		if p == j {
			return nil // already waiting
		}
	}
	s.Pending = append(s.Pending, j)
	if len(s.Pending) > maxPending {
		d := s.Pending[0]
		s.Pending = s.Pending[1:]
		return &d
	}
	return nil
}

// done removes j from the queue.
func (s *State) done(j Job) {
	for i, p := range s.Pending {
		if p == j {
			s.Pending = append(s.Pending[:i:i], s.Pending[i+1:]...)
			return
		}
	}
}
