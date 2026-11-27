package main

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestManifest checks holyarrs-plugin.json field by field, so a change to
// what the plugin asks for is always a deliberate one.
func TestManifest(t *testing.T) {
	b, err := os.ReadFile("holyarrs-plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		SupportURL    string `json:"support_url"`
		Version       string `json:"version"`
		Licence       string `json:"licence"`
		MinAppVersion string `json:"min_app_version"`
		Access        struct {
			Hosts   []string `json:"hosts"`
			Folders []struct {
				Name, Mode, Why string
				Required        bool
			} `json:"folders"`
			Actions []string `json:"actions"`
			Events  []string `json:"events"`
		} `json:"access"`
		Settings []struct {
			Key, Type string
			Default   json.RawMessage
		} `json:"settings"`
		LongDescription string   `json:"long_description"`
		HowItWorks      []string `json:"how_it_works"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "whisper-subtitles" || m.Name != "Whisper Subtitles" || m.Licence != "AGPL-3.0" || m.MinAppVersion != "1.3.1" || m.Version != "0.1.0" {
		t.Errorf("id/name/licence/min_app_version/version = %q %q %q %q %q", m.ID, m.Name, m.Licence, m.MinAppVersion, m.Version)
	}
	if m.SupportURL != "https://github.com/mandopatriot/whisper-subtitles/issues" {
		t.Errorf("support_url = %q", m.SupportURL)
	}
	if len(m.Access.Hosts) != 0 {
		t.Errorf("hosts = %v, want none: the Whisper server is an owner host", m.Access.Hosts)
	}
	if want := []string{"library.read", "files.subtitles.change", "sources.subtitles"}; !reflect.DeepEqual(m.Access.Actions, want) {
		t.Errorf("actions = %v, want %v", m.Access.Actions, want)
	}
	if want := []string{"library.subtitle"}; !reflect.DeepEqual(m.Access.Events, want) {
		t.Errorf("events = %v, want %v", m.Access.Events, want)
	}
	if len(m.Access.Folders) != 4 {
		t.Fatalf("folders = %+v, want movies, shows, more_movies and more_shows", m.Access.Folders)
	}
	for i, name := range []string{"movies", "shows", "more_movies", "more_shows"} {
		f := m.Access.Folders[i]
		if f.Name != name || f.Mode != "read" || f.Required || f.Why == "" {
			t.Errorf("folder %d = %+v, want %s, read only, optional, with a reason", i, f, name)
		}
	}
	want := []struct{ key, typ, def string }{
		{"asr_url", "text", `""`},
		{"transcribe", "toggle", strconv.Quote(strconv.FormatBool(defTranscribe))},
		{"sync", "toggle", strconv.Quote(strconv.FormatBool(defSync))},
		{"threshold_episodes", "number", strconv.Itoa(defThresholdEpisodes)},
		{"threshold_movies", "number", strconv.Itoa(defThresholdMovies)},
		{"jobs", "number", strconv.Itoa(defJobs)},
		{"job_timeout_minutes", "number", strconv.Itoa(defJobTimeoutMinutes)},
		{"check_titles", "text", `""`},
		{"check_only", "toggle", `"false"`},
	}
	if len(m.Settings) != len(want) {
		t.Fatalf("%d settings, want %d", len(m.Settings), len(want))
	}
	for i, w := range want {
		s := m.Settings[i]
		if s.Key != w.key || s.Type != w.typ || string(s.Default) != w.def {
			t.Errorf("setting %d = %s %s %s, want %s %s %s", i, s.Key, s.Type, s.Default, w.key, w.typ, w.def)
		}
	}
	// HolyArrs's page limits (plugins/manifest.go): 12 plain items of at most
	// 600 bytes, and 4000 for the long description.
	if len(m.HowItWorks) > 12 || len(m.LongDescription) > 4000 || strings.ContainsAny(m.LongDescription, "<>") {
		t.Errorf("how_it_works has %d items, long_description %d bytes", len(m.HowItWorks), len(m.LongDescription))
	}
	timing := false
	for _, s := range m.HowItWorks {
		if len(s) > 600 || strings.TrimSpace(s) == "" || strings.ContainsAny(s, "<>") {
			t.Errorf("how_it_works item too long or not plain: %q", s)
		}
		// The timing source is said plainly (CLAUDE.local.md): word timings
		// from Faster-Whisper based engines, phrases only from INTEL-WHISPER,
		// and the 1 second minimum that follows.
		timing = timing || strings.Contains(s, "word timings") && strings.Contains(s, "Faster-Whisper") &&
			strings.Contains(s, "INTEL-WHISPER") && strings.Contains(s, "1 second")
	}
	if !timing {
		t.Error("how_it_works doesn't explain word and phrase timings")
	}
	if fitMinShiftPhrasesMs != 1000 {
		t.Errorf("how_it_works says 1 second; fitMinShiftPhrasesMs is %d", fitMinShiftPhrasesMs)
	}
	// The numbers CLAUDE.local.md fixes.
	if defThresholdEpisodes != 96 || defThresholdMovies != 86 || defJobs != 1 || defJobTimeoutMinutes != 180 || !defTranscribe || !defSync {
		t.Error("defaults must be 96, 86, 1 job, 180 minutes, and both jobs on")
	}
	// The site is explained: the Plugin site address, and that only tagged
	// tracks are offered.
	site := false
	for _, s := range m.HowItWorks {
		site = site || strings.Contains(s, "plugin://whisper-subtitles")
	}
	if !site || !strings.Contains(m.LongDescription, "90-second") || chunkMs != 90_000 {
		t.Error("how_it_works or long_description doesn't explain the subtitle site and its 90-second pieces")
	}
}
