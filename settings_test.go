package main

import (
	"reflect"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	// m is keyed by the setting part, such as "JOBS".
	return func(k string) string { return m[strings.TrimPrefix(k, "HOLYARRS_PLUGIN_SETTING_")] }
}

func TestSettingsDefaults(t *testing.T) {
	s, warn := readSettings(env(nil))
	if len(warn) != 0 {
		t.Errorf("warnings %v", warn)
	}
	want := Settings{ThresholdEpisodes: 96, ThresholdMovies: 86, Jobs: 1, JobTimeoutMinutes: 180, Transcribe: true, Sync: true}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("got %+v, want %+v", s, want)
	}
}

func TestSettingsValues(t *testing.T) {
	s, warn := readSettings(env(map[string]string{
		"ASR_URL":             " http://whisper.example.lan:9000/ ",
		"THRESHOLD_EPISODES":  "90",
		"THRESHOLD_MOVIES":    "101",
		"JOBS":                "4",
		"JOB_TIMEOUT_MINUTES": "1440",
		"CHECK_TITLES":        "12, 34;12 x -3",
		"CHECK_ONLY":          "true",
	}))
	if s.ASRURL != "http://whisper.example.lan:9000" || s.ThresholdEpisodes != 90 || s.ThresholdMovies != 101 ||
		s.Jobs != 4 || s.JobTimeoutMinutes != 1440 || !s.CheckOnly {
		t.Errorf("got %+v", s)
	}
	if !reflect.DeepEqual(s.CheckTitles, []int64{12, 34}) || s.CheckTitlesRaw != "12, 34;12 x -3" {
		t.Errorf("check_titles = %v (%q)", s.CheckTitles, s.CheckTitlesRaw)
	}
	if len(warn) != 2 { // "x" and "-3"
		t.Errorf("warnings %v, want 2", warn)
	}
}

func TestSettingsOutOfRange(t *testing.T) {
	for _, c := range []struct{ key, val string }{
		{"THRESHOLD_EPISODES", "-1"}, {"THRESHOLD_EPISODES", "102"}, {"THRESHOLD_MOVIES", "85.5"},
		{"JOBS", "0"}, {"JOBS", "5"}, {"JOB_TIMEOUT_MINUTES", "0"}, {"JOB_TIMEOUT_MINUTES", "1441"},
		{"JOBS", "two"}, {"JOBS", "NaN"}, {"CHECK_ONLY", "yes"}, {"TRANSCRIBE", "on"}, {"SYNC", "1"},
		{"ASR_URL", "whisper:9000"}, {"ASR_URL", "ftp://whisper"}, {"ASR_URL", "http://u:p@whisper:9000"}, {"ASR_URL", "http://w:9000/?a=1"},
	} {
		s, warn := readSettings(env(map[string]string{c.key: c.val}))
		def := defaults()
		if len(warn) != 1 || !reflect.DeepEqual(s, def) {
			t.Errorf("%s=%q: got %+v, warnings %v; want defaults and one warning", c.key, c.val, s, warn)
		}
	}
}

func TestSettingsEdges(t *testing.T) {
	for _, c := range []struct {
		key, val string
		get      func(Settings) int
		want     int
	}{
		{"THRESHOLD_EPISODES", "0", func(s Settings) int { return s.ThresholdEpisodes }, 0},
		{"THRESHOLD_MOVIES", "101", func(s Settings) int { return s.ThresholdMovies }, 101},
		{"JOBS", "1", func(s Settings) int { return s.Jobs }, 1},
		{"JOBS", "4.0", func(s Settings) int { return s.Jobs }, 4},
		{"JOB_TIMEOUT_MINUTES", "1", func(s Settings) int { return s.JobTimeoutMinutes }, 1},
	} {
		s, warn := readSettings(env(map[string]string{c.key: c.val}))
		if got := c.get(s); got != c.want || len(warn) != 0 {
			t.Errorf("%s=%q: got %d (warnings %v), want %d", c.key, c.val, got, warn, c.want)
		}
	}
}

// The two jobs can be turned off one by one.
func TestSettingsToggles(t *testing.T) {
	s, warn := readSettings(env(map[string]string{"TRANSCRIBE": "false", "SYNC": "FALSE", "CHECK_ONLY": "True"}))
	if len(warn) != 0 || s.Transcribe || s.Sync || !s.CheckOnly {
		t.Errorf("got %+v, warnings %v", s, warn)
	}
	s, _ = readSettings(env(map[string]string{"TRANSCRIBE": "true", "SYNC": ""}))
	if !s.Transcribe || !s.Sync {
		t.Errorf("got %+v, want both on", s)
	}
}
